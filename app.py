import os
import signal
import subprocess
import threading
from pathlib import Path
import requests
from flask import Flask, render_template, request, jsonify
from vnc_helper import ensure_vnc_services, get_vnc_status

# Inicialización autónoma de los servicios gráficos VNC/X11
ensure_vnc_services()

app = Flask(__name__)

# Configuración y estado global
UPLOAD_LOCK = threading.Lock()
UPLOAD_PROCESS = None
UPLOAD_LOGS = []
LOGIN_PROCESS = None

TOKENS_DIR = Path("/app/tokens")
PRIMARY_ENV_PATH = TOKENS_DIR / ".env"
FALLBACK_ENV_PATH = Path("/app/.env")
ENV_PATH = PRIMARY_ENV_PATH
ENV_PATHS = [PRIMARY_ENV_PATH, FALLBACK_ENV_PATH, Path(".env")]

def get_env_paths():
    paths = []
    if TOKENS_DIR.exists() or os.access("/app", os.W_OK):
        try:
            TOKENS_DIR.mkdir(parents=True, exist_ok=True)
            paths.append(PRIMARY_ENV_PATH)
        except Exception:
            pass
    paths.append(FALLBACK_ENV_PATH)
    paths.append(Path(".env"))
    return paths

def get_env_vars():
    env_vars = os.environ.copy()
    for p in get_env_paths():
        try:
            if p.exists() and p.is_file():
                with open(p, 'r', encoding='utf-8') as f:
                    for line in f:
                        line = line.strip()
                        if line and not line.startswith('#') and '=' in line:
                            k, v = line.split('=', 1)
                            env_vars[k.strip()] = v.strip().strip('"').strip("'")
                break
        except Exception:
            pass
    return env_vars

def save_env_vars(jsid, vkey):
    content = f'MOVISTAR_JSESSIONID="{jsid}"\nMOVISTAR_VALIDATIONKEY="{vkey}"\n'
    for p in get_env_paths():
        try:
            p.parent.mkdir(parents=True, exist_ok=True)
            with open(p, 'w', encoding='utf-8') as f:
                f.write(content)
        except Exception:
            pass

def keepalive_worker():
    """Hilo demonio que realiza ping periódico a Movistar Cloud para evitar que JSESSIONID caduque por inactividad."""
    import time
    while True:
        time.sleep(300)  # Cada 5 minutos
        try:
            env = get_env_vars()
            jsid = env.get("MOVISTAR_JSESSIONID")
            vk = env.get("MOVISTAR_VALIDATIONKEY")
            if jsid and vk:
                s = requests.Session()
                s.cookies.set("JSESSIONID", jsid, domain="micloud.movistar.es", path="/")
                s.cookies.set("validationkey", vk, domain="micloud.movistar.es", path="/")
                s.headers.update({"User-Agent": "MovistarCloud-KeepAlive/1.0"})
                url = f"https://micloud.movistar.es/sapi/system/information?action=get&validationkey={vk}"
                r = s.get(url, timeout=15)
                if r.status_code == 200:
                    app.logger.debug("Keep-Alive heartbeat OK (sesión renovada).")
                elif r.status_code == 401:
                    app.logger.warning("Keep-Alive detectó sesión expirada (401).")
        except Exception as e:
            app.logger.debug(f"Keep-Alive ping error: {e}")

KEEPALIVE_THREAD = threading.Thread(target=keepalive_worker, daemon=True)
KEEPALIVE_THREAD.start()

def kill_process_tree(proc):
    """
    Termina de forma limpia y forzosa un proceso y todo su grupo/árbol de procesos.
    Utiliza killpg en Linux/Docker con SIGTERM y escalado a SIGKILL, con fallback para Windows.
    """
    if proc is None:
        return
    try:
        pid = proc.pid
        if hasattr(os, "killpg") and hasattr(os, "getpgid"):
            try:
                pgid = os.getpgid(pid)
                os.killpg(pgid, signal.SIGTERM)
            except (ProcessLookupError, OSError):
                return
            try:
                proc.wait(timeout=2)
            except (subprocess.TimeoutExpired, Exception):
                try:
                    os.killpg(pgid, signal.SIGKILL)
                except (ProcessLookupError, OSError):
                    pass
                try:
                    proc.wait(timeout=1)
                except Exception:
                    pass
        else:
            proc.terminate()
            try:
                proc.wait(timeout=2)
            except (subprocess.TimeoutExpired, Exception):
                try:
                    proc.kill()
                    proc.wait(timeout=1)
                except Exception:
                    pass
    except Exception as e:
        app.logger.error(f"Error terminando proceso: {e}")

def read_upload_logs(proc):
    global UPLOAD_PROCESS, UPLOAD_LOGS
    try:
        for line in proc.stdout:
            UPLOAD_LOGS.append(line.rstrip())
            if len(UPLOAD_LOGS) > 500:
                UPLOAD_LOGS.pop(0)
        proc.wait()
        UPLOAD_LOGS.append(f"--- Proceso finalizado con código {proc.returncode} ---")
    except Exception as e:
        UPLOAD_LOGS.append(f"Error en proceso: {str(e)}")
    finally:
        with UPLOAD_LOCK:
            if UPLOAD_PROCESS == proc:
                UPLOAD_PROCESS = None

@app.route("/health", methods=["GET"])
def health():
    return jsonify({"status": "ok"})

@app.route("/")
def index():
    env = get_env_vars()
    return render_template(
        "index.html", 
        jsessionid=env.get("MOVISTAR_JSESSIONID", ""),
        validationkey=env.get("MOVISTAR_VALIDATIONKEY", "")
    )

@app.route("/start", methods=["POST"])
def start():
    global UPLOAD_PROCESS, UPLOAD_LOGS
    with UPLOAD_LOCK:
        if UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None:
            return jsonify({"status": "error", "message": "Ya hay una subida en curso"}), 400

        data = request.json
        path = data.get("path", "").strip()
        recursive = data.get("recursive", False)
        workers = 3  # Fijo a 3 hilos concurrentes para estabilidad óptima con la API de Movistar
        
        jsid = data.get("jsessionid", "").strip()
        vkey = data.get("validationkey", "").strip()

        takeout = data.get("takeout", False)

        if not path:
            return jsonify({"status": "error", "message": "La ruta no puede estar vacía"}), 400
        if not jsid or not vkey:
            return jsonify({"status": "error", "message": "Faltan las cookies de sesión"}), 400

        save_env_vars(jsid, vkey)
        env_vars = get_env_vars()

        cmd = ["python", "-u", "/app/movistar_cloud_gallery.py", path, "--workers", str(workers)]
        if recursive:
            cmd.append("--recursive")
        if takeout:
            cmd.append("--takeout")

        UPLOAD_LOGS.clear()
        UPLOAD_LOGS.append(f"$ {' '.join(cmd)}")

        try:
            proc = subprocess.Popen(
                cmd, 
                stdout=subprocess.PIPE, 
                stderr=subprocess.STDOUT, 
                text=True,
                bufsize=1,
                env=env_vars,
                start_new_session=True
            )
            UPLOAD_PROCESS = proc
        except Exception as e:
            UPLOAD_LOGS.append(f"Error al iniciar proceso: {e}")
            return jsonify({"status": "error", "message": f"Error al iniciar proceso: {e}"}), 500

        thread = threading.Thread(target=read_upload_logs, args=(proc,))
        thread.daemon = True
        thread.start()

        return jsonify({"status": "ok", "message": "Subida iniciada"})

@app.route("/stop", methods=["POST"])
def stop():
    global UPLOAD_PROCESS, UPLOAD_LOGS
    with UPLOAD_LOCK:
        proc = UPLOAD_PROCESS
        if proc is not None and proc.poll() is None:
            UPLOAD_LOGS.append("--- Cancelando subida por el usuario... ---")
            kill_process_tree(proc)
            UPLOAD_LOGS.append("--- Proceso abortado por el usuario ---")
            UPLOAD_PROCESS = None
            return jsonify({"status": "ok", "message": "Proceso detenido con éxito"})
        else:
            UPLOAD_PROCESS = None
            return jsonify({"status": "ok", "message": "No hay proceso activo"})

@app.route("/logs", methods=["GET"])
def logs():
    is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
    return jsonify({
        "logs": UPLOAD_LOGS, 
        "is_running": is_running, 
        "running": is_running
    })

@app.route("/folders", methods=["GET"])
def get_folders():
    current_path = request.args.get("path", "/data")
    if not os.path.abspath(current_path).startswith("/data"):
        current_path = "/data"
        
    folders = []
    if os.path.exists(current_path):
        try:
            for entry in os.scandir(current_path):
                if entry.is_dir():
                    folders.append({"path": entry.path, "name": entry.name})
        except Exception:
            pass
            
    folders.sort(key=lambda x: x["name"].lower())
    parent = os.path.dirname(current_path) if current_path != "/data" else None
    return jsonify({"current_path": current_path, "parent": parent, "folders": folders})

@app.route("/debug_vnc", methods=["GET"])
def debug_vnc():
    status = get_vnc_status()
    logs = {}
    for log_file in ["/tmp/xvfb.log", "/tmp/fluxbox.log", "/tmp/x11vnc.log", "/tmp/websockify.log"]:
        try:
            with open(log_file, "r") as f:
                logs[log_file] = f.read()
        except Exception as e:
            logs[log_file] = str(e)
    return jsonify({"status": status, "logs": logs})

@app.route("/start_login", methods=["POST"])
def start_login():
    global LOGIN_PROCESS
    # Garantizar que Xvfb y VNC están activos antes de lanzar Playwright
    ensure_vnc_services()

    # Terminar proceso previo si existe
    if LOGIN_PROCESS is not None and LOGIN_PROCESS.poll() is None:
        kill_process_tree(LOGIN_PROCESS)
        LOGIN_PROCESS = None
            
    # Limpiar credenciales previas para que la detección sea limpia
    for p in get_env_paths():
        if p.exists():
            try:
                p.unlink()
            except Exception:
                pass

    LOGIN_PROCESS = subprocess.Popen(
        ["python", "-u", "/app/movistar_login.py"],
        start_new_session=True
    )
    return jsonify({"status": "ok"})

@app.route("/stop_login", methods=["POST"])
def stop_login():
    global LOGIN_PROCESS
    if LOGIN_PROCESS is not None and LOGIN_PROCESS.poll() is None:
        kill_process_tree(LOGIN_PROCESS)
        LOGIN_PROCESS = None
        return jsonify({"status": "ok", "message": "Login cancelado"})
    LOGIN_PROCESS = None
    return jsonify({"status": "ok", "message": "No había proceso de login activo"})

@app.route("/env", methods=["GET"])
def get_env():
    env = get_env_vars()
    return jsonify({
        "jsessionid": env.get("MOVISTAR_JSESSIONID", ""),
        "validationkey": env.get("MOVISTAR_VALIDATIONKEY", "")
    })

if __name__ == "__main__":
    ensure_vnc_services()
    app.run(host="0.0.0.0", port=5000)

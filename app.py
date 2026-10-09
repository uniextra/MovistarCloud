import os
import re
import signal
import subprocess
import threading
from pathlib import Path
import requests
from flask import Flask, render_template, request, jsonify, send_from_directory
from vnc_helper import ensure_vnc_services, get_vnc_status

# Inicialización autónoma de los servicios gráficos VNC/X11
ensure_vnc_services()

app = Flask(__name__)

# Configuración y estado global
UPLOAD_LOCK = threading.Lock()
UPLOAD_PROCESS = None
ACTIVE_UPLOAD_PHONE = None
UPLOAD_LOGS = []
LOGIN_PROCESS = None
LOGIN_PHONE = None

TOKENS_DIR = Path("/app/tokens")
PRIMARY_ENV_PATH = TOKENS_DIR / ".env"
FALLBACK_ENV_PATH = Path("/app/.env")

def sanitize_phone(phone):
    if not phone:
        return ""
    digits = re.sub(r"\D", "", str(phone).strip())
    if len(digits) == 11 and digits.startswith("34"):
        digits = digits[2:]
    return digits

def get_account_env_path(phone):
    phone_clean = sanitize_phone(phone)
    if not phone_clean:
        return None
    acct_file = TOKENS_DIR / f"account_{phone_clean}.env"
    if acct_file.exists():
        return acct_file
    legacy_file = TOKENS_DIR / f"{phone_clean}.env"
    if legacy_file.exists():
        return legacy_file
    return acct_file

def get_env_paths(phone=None):
    paths = []
    phone_clean = sanitize_phone(phone)
    if phone_clean:
        acct_file = TOKENS_DIR / f"account_{phone_clean}.env"
        legacy_file = TOKENS_DIR / f"{phone_clean}.env"
        if acct_file.exists():
            paths.append(acct_file)
        if legacy_file.exists():
            paths.append(legacy_file)
        if not paths:
            paths.append(acct_file)
        return paths

    if TOKENS_DIR.exists() or os.access("/app", os.W_OK):
        try:
            TOKENS_DIR.mkdir(parents=True, exist_ok=True)
            paths.append(PRIMARY_ENV_PATH)
        except Exception:
            pass
    paths.append(FALLBACK_ENV_PATH)
    paths.append(Path(".env"))
    return paths

def get_env_vars(phone=None):
    phone_clean = sanitize_phone(phone)
    parsed_vars = {}

    if phone_clean:
        # Modo estricto por teléfono: buscar únicamente en los archivos de este número
        candidates = [
            TOKENS_DIR / f"account_{phone_clean}.env",
            TOKENS_DIR / f"{phone_clean}.env"
        ]
        for p in candidates:
            try:
                if p.exists() and p.is_file():
                    with open(p, 'r', encoding='utf-8') as f:
                        for line in f:
                            line = line.strip()
                            if line and not line.startswith('#') and '=' in line:
                                k, v = line.split('=', 1)
                                parsed_vars[k.strip()] = v.strip().strip('"').strip("'")
                    if parsed_vars.get("MOVISTAR_JSESSIONID") or parsed_vars.get("MOVISTAR_VALIDATIONKEY"):
                        return parsed_vars
            except Exception:
                pass
        return parsed_vars

    # Si no se indica teléfono: fallback a archivos globales
    for p in [PRIMARY_ENV_PATH, FALLBACK_ENV_PATH, Path(".env")]:
        try:
            if p.exists() and p.is_file():
                with open(p, 'r', encoding='utf-8') as f:
                    for line in f:
                        line = line.strip()
                        if line and not line.startswith('#') and '=' in line:
                            k, v = line.split('=', 1)
                            parsed_vars[k.strip()] = v.strip().strip('"').strip("'")
                if parsed_vars.get("MOVISTAR_JSESSIONID") or parsed_vars.get("MOVISTAR_VALIDATIONKEY"):
                    return parsed_vars
        except Exception:
            pass
    return parsed_vars

def save_env_vars(jsid, vkey, phone=None):
    phone_clean = sanitize_phone(phone)
    content = f'MOVISTAR_JSESSIONID="{jsid}"\nMOVISTAR_VALIDATIONKEY="{vkey}"\n'
    if phone_clean:
        content += f'MOVISTAR_PHONE="{phone_clean}"\n'
        targets = [TOKENS_DIR / f"account_{phone_clean}.env"]
    else:
        targets = [PRIMARY_ENV_PATH, FALLBACK_ENV_PATH]

    for p in targets:
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
            # Buscar todos los archivos de cuentas disponibles
            env_files = [PRIMARY_ENV_PATH, FALLBACK_ENV_PATH]
            if TOKENS_DIR.exists():
                env_files.extend(list(TOKENS_DIR.glob("account_*.env")))
                env_files.extend(list(TOKENS_DIR.glob("*.env")))

            checked_keys = set()
            for env_path in env_files:
                if not (env_path.exists() and env_path.is_file()):
                    continue
                try:
                    jsid, vk = "", ""
                    with open(env_path, 'r', encoding='utf-8') as f:
                        for line in f:
                            if "MOVISTAR_JSESSIONID=" in line:
                                jsid = line.split("=", 1)[1].strip().strip('"').strip("'")
                            elif "MOVISTAR_VALIDATIONKEY=" in line:
                                vk = line.split("=", 1)[1].strip().strip('"').strip("'")
                    if jsid and vk and vk not in checked_keys:
                        checked_keys.add(vk)
                        s = requests.Session()
                        s.cookies.set("JSESSIONID", jsid, domain="micloud.movistar.es", path="/")
                        s.cookies.set("validationkey", vk, domain="micloud.movistar.es", path="/")
                        s.headers.update({"User-Agent": "MovistarCloud-KeepAlive/1.0"})
                        url = f"https://micloud.movistar.es/sapi/system/information?action=get&validationkey={vk}"
                        r = s.get(url, timeout=15)
                        if r.status_code == 200:
                            app.logger.debug(f"Keep-Alive heartbeat OK para {env_path.name}")
                except Exception:
                    pass
        except Exception as e:
            app.logger.debug(f"Keep-Alive ping error: {e}")
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

@app.route("/favicon.ico")
def favicon():
    return send_from_directory(os.path.join(app.root_path, "static"), "favicon.ico", mimetype="image/vnd.microsoft.icon")

@app.route("/")
def index():
    # Se renderiza la interfaz limpia sin filtrar credenciales en el HTML inicial
    return render_template("index.html")

@app.route("/verify_phone", methods=["POST"])
def verify_phone():
    global ACTIVE_UPLOAD_PHONE, UPLOAD_PROCESS
    data = request.json or {}
    raw_phone = data.get("phone", "").strip()
    phone = sanitize_phone(raw_phone)
    if not phone or len(phone) < 6:
        return jsonify({"status": "error", "message": "Número de teléfono no válido"}), 400

    env = get_env_vars(phone)
    jsid = env.get("MOVISTAR_JSESSIONID", "")
    vk = env.get("MOVISTAR_VALIDATIONKEY", "")

    # Chequeo de subidas en curso
    is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
    is_running_this = is_running and (ACTIVE_UPLOAD_PHONE == phone)
    another_running = is_running and (ACTIVE_UPLOAD_PHONE != phone)

    return jsonify({
        "status": "ok",
        "phone": phone,
        "has_credentials": bool(jsid and vk),
        "jsessionid": jsid,
        "validationkey": vk,
        "is_running_this": is_running_this,
        "another_running": another_running,
        "active_running_phone": ACTIVE_UPLOAD_PHONE if another_running else None
    })

@app.route("/start", methods=["POST"])
def start():
    global UPLOAD_PROCESS, UPLOAD_LOGS, ACTIVE_UPLOAD_PHONE
    with UPLOAD_LOCK:
        data = request.json or {}
        phone = sanitize_phone(data.get("phone", ""))
        if not phone:
            return jsonify({"status": "error", "message": "Debes especificar un número móvil para la cuenta"}), 400

        is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
        if is_running:
            if ACTIVE_UPLOAD_PHONE != phone:
                return jsonify({
                    "status": "error", 
                    "message": f"Ya hay una subida en curso para el número {ACTIVE_UPLOAD_PHONE}. Espera a que termine o cancélala antes de iniciar otra cuenta."
                }), 400
            else:
                return jsonify({"status": "error", "message": "Ya hay una subida en curso para esta cuenta"}), 400

        path = data.get("path", "").strip()
        recursive = data.get("recursive", False)
        workers = 3  # Fijo a 3 hilos concurrentes para estabilidad óptima con la API de Movistar
        
        jsid = data.get("jsessionid", "").strip()
        vkey = data.get("validationkey", "").strip()
        takeout = data.get("takeout", False)

        if not path:
            return jsonify({"status": "error", "message": "La ruta no puede estar vacía"}), 400
        if not jsid or not vkey:
            return jsonify({"status": "error", "message": "Faltan las credenciales de sesión"}), 400

        save_env_vars(jsid, vkey, phone=phone)
        proc_env = os.environ.copy()
        proc_env.update(get_env_vars(phone))
        proc_env["MOVISTAR_JSESSIONID"] = jsid
        proc_env["MOVISTAR_VALIDATIONKEY"] = vkey
        proc_env["MOVISTAR_PHONE"] = phone

        cmd = [
            "python", "-u", "/app/movistar_cloud_gallery.py", 
            path, 
            "--workers", str(workers),
            "--phone", phone
        ]
        if recursive:
            cmd.append("--recursive")
        if takeout:
            cmd.append("--takeout")

        UPLOAD_LOGS.clear()
        UPLOAD_LOGS.append(f"$ {' '.join(cmd)}")
        ACTIVE_UPLOAD_PHONE = phone

        try:
            proc = subprocess.Popen(
                cmd, 
                stdout=subprocess.PIPE, 
                stderr=subprocess.STDOUT, 
                text=True,
                bufsize=1,
                env=proc_env,
                start_new_session=True
            )
            UPLOAD_PROCESS = proc
        except Exception as e:
            UPLOAD_LOGS.append(f"Error al iniciar proceso: {e}")
            ACTIVE_UPLOAD_PHONE = None
            return jsonify({"status": "error", "message": f"Error al iniciar proceso: {e}"}), 500

        thread = threading.Thread(target=read_upload_logs, args=(proc,))
        thread.daemon = True
        thread.start()

        return jsonify({"status": "ok", "message": "Subida iniciada"})

@app.route("/stop", methods=["POST"])
def stop():
    global UPLOAD_PROCESS, UPLOAD_LOGS, ACTIVE_UPLOAD_PHONE
    with UPLOAD_LOCK:
        proc = UPLOAD_PROCESS
        if proc is not None and proc.poll() is None:
            UPLOAD_LOGS.append("--- Cancelando subida por el usuario... ---")
            kill_process_tree(proc)
            UPLOAD_LOGS.append("--- Proceso abortado por el usuario ---")
            UPLOAD_PROCESS = None
            ACTIVE_UPLOAD_PHONE = None
            return jsonify({"status": "ok", "message": "Proceso detenido con éxito"})
        else:
            UPLOAD_PROCESS = None
            ACTIVE_UPLOAD_PHONE = None
            return jsonify({"status": "ok", "message": "No hay proceso activo"})

@app.route("/logs", methods=["GET"])
def logs():
    phone = sanitize_phone(request.args.get("phone", ""))
    is_running = UPLOAD_PROCESS is not None and UPLOAD_PROCESS.poll() is None
    
    if is_running and phone and ACTIVE_UPLOAD_PHONE and ACTIVE_UPLOAD_PHONE != phone:
        return jsonify({
            "logs": [f"--- Hay una subida en curso activa para otra cuenta ({ACTIVE_UPLOAD_PHONE}) ---"],
            "is_running": False,
            "running": False,
            "another_running": True,
            "active_running_phone": ACTIVE_UPLOAD_PHONE
        })

    return jsonify({
        "logs": UPLOAD_LOGS, 
        "is_running": is_running, 
        "running": is_running,
        "active_phone": ACTIVE_UPLOAD_PHONE,
        "another_running": False
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
    global LOGIN_PROCESS, LOGIN_PHONE
    data = request.json or {}
    phone = sanitize_phone(data.get("phone", ""))
    LOGIN_PHONE = phone

    ensure_vnc_services()

    if LOGIN_PROCESS is not None and LOGIN_PROCESS.poll() is None:
        kill_process_tree(LOGIN_PROCESS)
        LOGIN_PROCESS = None
            
    cmd = ["python", "-u", "/app/movistar_login.py"]
    if phone:
        cmd.extend(["--phone", phone])

    LOGIN_PROCESS = subprocess.Popen(cmd, start_new_session=True)
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
    phone = sanitize_phone(request.args.get("phone", ""))
    if not phone:
        return jsonify({"phone": "", "jsessionid": "", "validationkey": ""})
    env = get_env_vars(phone)
    return jsonify({
        "phone": phone,
        "jsessionid": env.get("MOVISTAR_JSESSIONID", ""),
        "validationkey": env.get("MOVISTAR_VALIDATIONKEY", "")
    })

if __name__ == "__main__":
    ensure_vnc_services()
    app.run(host="0.0.0.0", port=5000)

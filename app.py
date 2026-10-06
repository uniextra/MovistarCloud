import os
import subprocess
import threading
from flask import Flask, render_template, request, jsonify
from vnc_helper import ensure_vnc_services, get_vnc_status

# Inicialización autónoma de los servicios gráficos VNC/X11
ensure_vnc_services()

app = Flask(__name__)

# Configuración y estado global
UPLOAD_PROCESS = None
UPLOAD_LOGS = []
LOGIN_PROCESS = None
ENV_PATH = "/app/.env"

def background_upload(cmd, env_vars):
    global UPLOAD_PROCESS, UPLOAD_LOGS
    UPLOAD_LOGS.clear()
    UPLOAD_LOGS.append(f"$ {' '.join(cmd)}")
    
    try:
        UPLOAD_PROCESS = subprocess.Popen(
            cmd, 
            stdout=subprocess.PIPE, 
            stderr=subprocess.STDOUT, 
            text=True,
            env=env_vars
        )
        for line in UPLOAD_PROCESS.stdout:
            UPLOAD_LOGS.append(line.rstrip())
            if len(UPLOAD_LOGS) > 500:
                UPLOAD_LOGS.pop(0)
        UPLOAD_PROCESS.wait()
        UPLOAD_LOGS.append(f"--- Proceso finalizado con código {UPLOAD_PROCESS.returncode} ---")
    except Exception as e:
        UPLOAD_LOGS.append(f"Error al iniciar: {str(e)}")
    finally:
        UPLOAD_PROCESS = None

def get_env_vars():
    env_vars = os.environ.copy()
    if os.path.exists(ENV_PATH):
        with open(ENV_PATH, 'r', encoding='utf-8') as f:
            for line in f:
                line = line.strip()
                if line and not line.startswith('#') and '=' in line:
                    k, v = line.split('=', 1)
                    env_vars[k.strip()] = v.strip().strip('"').strip("'")
    return env_vars

def save_env_vars(jsid, vkey):
    with open(ENV_PATH, 'w', encoding='utf-8') as f:
        f.write(f'MOVISTAR_JSESSIONID="{jsid}"\n')
        f.write(f'MOVISTAR_VALIDATIONKEY="{vkey}"\n')

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
    global UPLOAD_PROCESS
    if UPLOAD_PROCESS is not None:
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

    cmd = ["python", "/app/movistar_cloud_gallery.py", path, "--workers", str(workers)]
    if recursive:
        cmd.append("--recursive")
    if takeout:
        cmd.append("--takeout")

    thread = threading.Thread(target=background_upload, args=(cmd, env_vars))
    thread.daemon = True
    thread.start()

    return jsonify({"status": "ok", "message": "Subida iniciada"})

@app.route("/stop", methods=["POST"])
def stop():
    global UPLOAD_PROCESS, UPLOAD_LOGS
    if UPLOAD_PROCESS is not None:
        try:
            UPLOAD_PROCESS.terminate()
            UPLOAD_PROCESS.wait(timeout=2)
        except Exception:
            try:
                UPLOAD_PROCESS.kill()
            except Exception:
                pass
        UPLOAD_LOGS.append("--- Proceso abortado por el usuario ---")
        UPLOAD_PROCESS = None
        return jsonify({"status": "ok"})
    return jsonify({"status": "error", "message": "No hay proceso en curso"})

@app.route("/logs", methods=["GET"])
def logs():
    return jsonify({"logs": UPLOAD_LOGS, "is_running": UPLOAD_PROCESS is not None, "running": UPLOAD_PROCESS is not None})

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
        try:
            LOGIN_PROCESS.terminate()
            LOGIN_PROCESS.wait(timeout=2)
        except Exception:
            LOGIN_PROCESS.kill()
            
    # Limpiar credenciales previas para que la detección sea limpia
    if os.path.exists(ENV_PATH):
        try:
            os.remove(ENV_PATH)
        except Exception:
            pass

    LOGIN_PROCESS = subprocess.Popen(["python", "-u", "/app/movistar_login.py"])
    return jsonify({"status": "ok"})

@app.route("/stop_login", methods=["POST"])
def stop_login():
    global LOGIN_PROCESS
    if LOGIN_PROCESS is not None and LOGIN_PROCESS.poll() is None:
        try:
            LOGIN_PROCESS.terminate()
            LOGIN_PROCESS.wait(timeout=2)
        except Exception:
            LOGIN_PROCESS.kill()
        LOGIN_PROCESS = None
        return jsonify({"status": "ok", "message": "Login cancelado"})
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

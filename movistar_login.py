import time
import signal
import os
from pathlib import Path
from playwright.sync_api import sync_playwright

def handle_sigterm(signum, frame):
    os._exit(0)

try:
    signal.signal(signal.SIGTERM, handle_sigterm)
    signal.signal(signal.SIGINT, handle_sigterm)
except Exception:
    pass

try:
    from vnc_helper import ensure_vnc_services
    ensure_vnc_services()
except Exception as e:
    pass

def get_movistar_cookies():
    try:
        from vnc_helper import ensure_vnc_services
        ensure_vnc_services()
    except Exception:
        pass

    print("Abriendo navegador para iniciar sesión en Movistar Cloud...", flush=True)
    print("Por favor, inicia sesión normalmente (con tu teléfono y SMS).", flush=True)
    print("Esta ventana se cerrará automáticamente en cuanto inicies sesión correctamente.", flush=True)
    
    # Aseguramos que Playwright use la pantalla virtual Xvfb
    os.environ["DISPLAY"] = ":99"
    
    with sync_playwright() as p:
        # Detectar ejecutable del sistema para optimizar espacio si está presente
        exec_path = "/usr/bin/chromium" if os.path.exists("/usr/bin/chromium") else None

        browser = p.chromium.launch(
            headless=False,
            executable_path=exec_path,
            args=["--start-maximized", "--no-sandbox", "--disable-dev-shm-usage"]
        )
        context = browser.new_context(no_viewport=True)
        page = context.new_page()
        
        # Navegamos a la página principal
        page.goto("https://micloud.movistar.es")
        
        jsessionid = None
        validationkey = None
        start_time = time.time()
        timeout_seconds = 600  # 10 minutos de timeout máximo
        
        # Bucle de espera activa hasta que detectemos las cookies de sesión
        while True:
            if time.time() - start_time > timeout_seconds:
                print("Tiempo de espera agotado (10 min). Cerrando navegador...", flush=True)
                browser.close()
                return None, None

            # Obtenemos TODAS las cookies de cualquier dominio (micloud.movistar.es, telefonica, etc.)
            cookies = context.cookies()
            
            for c in cookies:
                c_name = c.get('name', '').lower()
                if c_name == 'jsessionid':
                    jsessionid = c['value']
                elif c_name in ('validationkey', 'validation_key', 'vk'):
                    validationkey = c['value']

            # En caso de que validationkey esté en sessionStorage, localStorage o en la URL
            if not validationkey or not jsessionid:
                try:
                    current_url = page.url
                    if "validationkey=" in current_url:
                        import urllib.parse
                        parsed = urllib.parse.parse_qs(urllib.parse.urlparse(current_url).query)
                        if "validationkey" in parsed:
                            validationkey = parsed["validationkey"][0]

                    storage_vk = page.evaluate("() => localStorage.getItem('validationkey') || sessionStorage.getItem('validationkey') || ''")
                    if storage_vk:
                        validationkey = storage_vk
                except Exception:
                    pass
            
            # Si tenemos ambas credenciales, el usuario ha completado el inicio de sesión
            if jsessionid and validationkey:
                print(f"[login] Capturadas credenciales: JSESSIONID={jsessionid[:6]}... VK={validationkey[:6]}...", flush=True)
                break
                
            # Si el usuario cierra el navegador manualmente, abortamos
            if len(context.pages) == 0:
                print("Se cerró el navegador antes de completar el login.", flush=True)
                return None, None
                
            time.sleep(1)
            
        print("\n¡Login detectado con éxito!", flush=True)
        browser.close()
        return jsessionid, validationkey

import argparse
import re

def sanitize_phone(phone: str) -> str:
    if not phone:
        return ""
    digits = re.sub(r"\D", "", str(phone).strip())
    if len(digits) == 11 and digits.startswith("34"):
        digits = digits[2:]
    return digits

def save_env(jsessionid, validationkey, phone=None):
    phone_clean = sanitize_phone(phone)
    if not phone_clean:
        print("Error: No se proporcionó número de teléfono para guardar credenciales.", flush=True)
        return

    content = f"""# Movistar Cloud Session
MOVISTAR_JSESSIONID="{jsessionid}"
MOVISTAR_VALIDATIONKEY="{validationkey}"
MOVISTAR_PHONE="{phone_clean}"
"""
    target = Path(f"/app/tokens/account_{phone_clean}.env")
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content, encoding='utf-8')
        print(f"Archivo de credenciales guardado correctamente en: {target}", flush=True)
    except Exception as e:
        print(f"Error al guardar credenciales: {e}", flush=True)

if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--phone", type=str, default=None, help="Número de teléfono móvil asociado")
    args, _ = parser.parse_known_args()

    jsessionid, validationkey = get_movistar_cookies()
    if jsessionid and validationkey:
        save_env(jsessionid, validationkey, phone=args.phone)
        print("Ya puedes ejecutar el script de subida. ¡Tus credenciales están listas!", flush=True)
    else:
        print("No se pudo obtener la sesión.", flush=True)

#!/usr/bin/env python3

from __future__ import annotations

import argparse
import json
import logging
import mimetypes
import os
import sys
import time
import uuid
import hashlib
from datetime import datetime, timezone
from pathlib import Path

import requests

try:
    from PIL import Image
    from PIL.ExifTags import TAGS
except ImportError:
    Image = None

# ---------------------------------------------------------
# CONSTANTS & CONFIGURATION
# ---------------------------------------------------------
BASE_URL = "https://micloud.movistar.es"
UPLOAD_URL = "https://upload.micloud.movistar.es"
DEVICE_FILE = Path.home() / ".movistar-cloud-device-id"

LOGIN_PATH = "/sapi/login?action=login"
VALIDATION_PATH = "/sapi/media?action=get-validation-status"

PHOTO_EXTENSIONS = {
    ".jpg", ".jpeg", ".jpe", ".png", ".gif", ".webp",
    ".heic", ".heif", ".tif", ".tiff", ".bmp", ".avif"
}
VIDEO_EXTENSIONS = {
    ".mp4", ".mov", ".m4v", ".3gp", ".avi", ".mkv", ".webm"
}

logger = logging.getLogger("MovistarGallery")

def die(msg: str, code: int = 1) -> None:
    logger.error(msg)
    sys.exit(code)

def get_device_id() -> str:
    if DEVICE_FILE.exists():
        value = DEVICE_FILE.read_text(encoding="utf-8").strip()
        if value: return value
    value = str(uuid.uuid4())
    DEVICE_FILE.write_text(value + "\n", encoding="utf-8")
    return value

def mime_for(path: Path) -> str:
    ext = path.suffix.lower()
    known = {
        ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg",
        ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
        ".heic": "image/heic", ".heif": "image/heif", ".tif": "image/tiff",
        ".tiff": "image/tiff", ".bmp": "image/bmp", ".avif": "image/avif",
        ".mp4": "video/mp4", ".mov": "video/quicktime", ".m4v": "video/x-m4v",
        ".3gp": "video/3gpp", ".avi": "video/x-msvideo", ".mkv": "video/x-matroska",
        ".webm": "video/webm",
    }
    if ext in known:
        return known[ext]
    return mimetypes.guess_type(path.name)[0] or "application/octet-stream"

def capture_time(path: Path, is_takeout: bool = False) -> datetime:
    """Attempts to extract original capture date from JSON (if Takeout) or EXIF, otherwise falls back to mtime."""
    
    # Modo Takeout: Buscar primero el metadata JSON original (Ej: foto.jpg.json o foto.json)
    if is_takeout:
        json_paths = [
            path.parent / (path.name + ".json"),
            path.parent / (path.stem + ".json"),
            path.parent / (path.stem + path.suffix[:2] + ".json") # Sometimes takeout cuts extensions
        ]
        
        for jp in json_paths:
            if jp.exists():
                try:
                    with open(jp, 'r', encoding='utf-8') as f:
                        data = json.load(f)
                        if "photoTakenTime" in data and "timestamp" in data["photoTakenTime"]:
                            ts = int(data["photoTakenTime"]["timestamp"])
                            return datetime.fromtimestamp(ts, tz=timezone.utc)
                except Exception:
                    pass

    # Fallback normal a EXIF
    if Image is not None and path.suffix.lower() in PHOTO_EXTENSIONS:
        try:
            with Image.open(path) as img:
                exif = img.getexif()
                if exif:
                    # Check ExifIFD (0x8769) first
                    exif_ifd = exif.get_ifd(0x8769)
                    raw = exif_ifd.get(36867) or exif_ifd.get(36868)
                    
                    # Fallback to standard IFD0
                    if not raw:
                        raw = exif.get(36867) or exif.get(36868) or exif.get(306)
                        
                    if raw:
                        dt = datetime.strptime(str(raw).strip().rstrip('\x00'), "%Y:%m:%d %H:%M:%S")
                        return dt.replace(tzinfo=timezone.utc)
        except Exception as e:
            logger.debug(f"Could not parse EXIF for {path.name}: {e}")
            
    return datetime.fromtimestamp(path.stat().st_mtime, tz=timezone.utc)

def api_date(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


class MovistarCloud:
    def __init__(self, email: str = None, password: str = None, jsessionid: str = None, validation_key: str = None, timeout: float = 60.0):
        self.email = email
        self.password = password
        self.timeout = timeout
        self.device_id = get_device_id()
        self.s = requests.Session()
        self.s.headers.update({
            "User-Agent": "MovistarCloud-GalleryUploader/1.0 (ReverseEngineered)",
            "Accept": "application/json, text/plain, */*",
        })
        self.validation_key = validation_key
        
        # If user provides direct SSO cookies (JSESSIONID / validationkey) to bypass SMS
        if jsessionid and validation_key:
            self.s.cookies.set("JSESSIONID", jsessionid, domain="micloud.movistar.es", path="/")
            self.s.cookies.set("validationkey", validation_key, domain="micloud.movistar.es", path="/")

    def _vk_url(self, url: str) -> str:
        sep = "&" if "?" in url else "?"
        return f"{url}{sep}validationkey={requests.utils.quote(self.validation_key or '', safe='')}"

    def login(self) -> None:
        """Logs in via email/password if cookies were not provided."""
        if self.s.cookies.get("JSESSIONID") and self.validation_key:
            logger.debug("Utilizando sesión pre-inyectada (cookies).")
            return
            
        if not self.email or not self.password:
            die("Faltan credenciales (Email/Password o JSESSIONID/ValidationKey).")

        logger.debug(f"Haciendo login tradicional con email: {self.email}")
        r = self.s.get(f"{BASE_URL}/", timeout=self.timeout)
        r.raise_for_status()

        r = self.s.post(
            f"{BASE_URL}{LOGIN_PATH}",
            data={"login": self.email, "password": self.password},
            headers={"X-Deviceid": self.device_id},
            timeout=self.timeout
        )
        r.raise_for_status()
        
        body = r.json()
        data = body.get("data") or {}
        jsessionid = data.get("jsessionid")
        self.validation_key = data.get("validationkey")

        if not jsessionid or not self.validation_key:
            raise RuntimeError("Login exitoso pero el servidor no devolvió jsessionid/validationkey. ¿Quizá tu cuenta requiere SMS? Usa MOVISTAR_JSESSIONID en su lugar.")

        self.s.cookies.set("JSESSIONID", jsessionid, domain="micloud.movistar.es", path="/")
        self.s.cookies.set("validationkey", self.validation_key, domain="micloud.movistar.es", path="/")

    def get_all_gallery_items(self) -> dict:
        """
        Fetches the complete gallery (pictures & videos) to prevent duplicate uploads.
        Movistar uses pagination/limits. We request up to 50000 items.
        """
        logger.debug("Obteniendo inventario de la Galería para control de duplicados...")
        existing_items = {}
        
        for media_type in ["picture", "video"]:
            url = self._vk_url(f"{BASE_URL}/sapi/media/{media_type}?action=get&limit=50000")
            payload = {
                "data": {
                    # Traemos size y origin para identificar correctamente la foto
                    "fields": ["name", "size", "folder", "origin"]
                }
            }
            try:
                r = self.s.post(url, json=payload, timeout=self.timeout)
                r.raise_for_status()
                # The response structure has data -> pictures (or videos) depending on the endpoint
                key = "pictures" if media_type == "picture" else "videos"
                items = r.json().get("data", {}).get(key, [])
                
                for item in items:
                    name = item.get("name")
                    size = item.get("size")
                    if name is not None and size is not None:
                        # Composite key: (filename, filesize)
                        existing_items[(name, int(size))] = item
                        
            except Exception as e:
                logger.warning(f"No se pudo cargar el inventario de {media_type}: {e}")

        logger.debug(f"Inventario cargado: {len(existing_items)} elementos detectados en la nube.")
        return existing_items

    def upload_to_gallery(self, path: Path, is_takeout: bool = False) -> dict:
        """
        REVERSE ENGINEERING FINDING:
        To upload an item so it behaves EXACTLY like the Android Gallery app
        (i.e., it appears in Galería but NOT as a generic file in a folder, 
        so deleting it from Ficheros is impossible):
        We must OMIT the "folderid" field entirely from the metadata JSON.
        """
        stat = path.stat()
        dt = capture_time(path, is_takeout=is_takeout)
        mime = mime_for(path)

        meta = {
            "data": {
                "name": path.name,
                "size": stat.st_size,
                "creationdate": api_date(dt),
                "modificationdate": api_date(dt),
                "contenttype": mime,
                # CRÍTICO: No enviar 'folderid'. Esto inyecta el objeto puramente en Galería.
            }
        }

        url = self._vk_url(f"{UPLOAD_URL}/sapi/upload?action=save&acceptasynchronous=true")
        logger.debug(f"Subiendo {path.name} ({mime}) - {stat.st_size} bytes - Date: {dt}")

        with path.open("rb") as fh:
            files = {
                "data": (None, json.dumps(meta), "application/json"),
                "file": (path.name, fh, mime),
            }
            r = self.s.post(url, files=files, timeout=None)

        r.raise_for_status()
        result = r.json()
        logger.debug(f"Respuesta subida: {result}")
        return {
            "id": int(result["id"]),
            "status": result.get("status", ""),
            "etag": result.get("etag", ""),
            "mime": mime,
            "date": dt.isoformat(),
        }

    def validation_status(self, file_id: int) -> str:
        payload = {"data": {"ids": [{"id": file_id}]}}
        url = self._vk_url(f"{BASE_URL}{VALIDATION_PATH}")
        r = self.s.post(url, json=payload, timeout=self.timeout)
        r.raise_for_status()
        ids = r.json().get("data", {}).get("ids", [])
        return ids[0].get("status", "") if ids else ""

    def wait_usable(self, file_id: int, timeout_s: int = 600) -> str:
        deadline = time.time() + timeout_s
        last = ""
        while time.time() < deadline:
            last = self.validation_status(file_id)
            if last.upper() == "U":
                return last
            time.sleep(3)
        return last


def iter_media_files(root: Path, recursive: bool):
    if root.is_file():
        if root.suffix.lower() in PHOTO_EXTENSIONS | VIDEO_EXTENSIONS:
            yield root
        return

    iterator = root.rglob("*") if recursive else root.glob("*")
    for p in sorted(iterator):
        if p.is_file() and p.suffix.lower() in PHOTO_EXTENSIONS | VIDEO_EXTENSIONS:
            yield p


def main() -> int:
    parser = argparse.ArgumentParser(description="Movistar Cloud Gallery Uploader")
    parser.add_argument("path", type=Path, help="Ruta de la imagen, vídeo o directorio a subir")
    parser.add_argument("--recursive", action="store_true", help="Buscar fotos en subcarpetas")
    parser.add_argument("--dry-run", action="store_true", help="Simular sin subir nada")
    parser.add_argument("--no-wait", action="store_true", help="No esperar validación tras subir")
    parser.add_argument("--debug", action="store_true", help="Mostrar logs HTTP y debug")
    parser.add_argument("--workers", type=int, default=3, help="Número de subidas simultáneas (por defecto: 3)")
    parser.add_argument("--takeout", action="store_true", help="Modo Google Takeout: omite miniaturas (<40KB) y lee el .json para las fechas")
    args = parser.parse_args()

    # Configuración de Logging
    log_level = logging.DEBUG if args.debug else logging.INFO
    logging.basicConfig(level=log_level, format="%(asctime)s [%(levelname)s] %(message)s", datefmt="%H:%M:%S")

    # Intentar cargar .env desde la carpeta donde está el script o desde el directorio actual
    env_paths = [Path(__file__).parent / ".env", Path.cwd() / ".env"]
    for ep in env_paths:
        if ep.exists():
            logger.debug(f"Cargando variables de entorno desde {ep}")
            with open(ep, 'r', encoding='utf-8') as f:
                for line in f:
                    line = line.strip()
                    if line and not line.startswith('#') and '=' in line:
                        k, v = line.split('=', 1)
                        os.environ[k.strip()] = v.strip().strip('"').strip("'")
            break

    # Autenticación: Soporta SSO (Cookies) o Tradicional (Email)
    email = os.getenv("MOVISTAR_EMAIL")
    password = os.getenv("MOVISTAR_PASSWORD")
    jsessionid = os.getenv("MOVISTAR_JSESSIONID")
    validationkey = os.getenv("MOVISTAR_VALIDATIONKEY")

    if not ((email and password) or (jsessionid and validationkey)):
        die("Debes definir variables de entorno: (MOVISTAR_EMAIL y MOVISTAR_PASSWORD) o (MOVISTAR_JSESSIONID y MOVISTAR_VALIDATIONKEY)")

    path = args.path.expanduser().resolve()
    if not path.exists():
        die(f"No existe: {path}")

    all_files = list(iter_media_files(path, args.recursive))
    
    # Filtrado especial Takeout
    if args.takeout:
        logger.info("Modo Takeout activado: Filtrando miniaturas y leyendo .json")
        # Filtramos ficheros que ocupen menos de 40KB o tengan 'thumbnail' en el nombre
        files = [
            f for f in all_files 
            if f.stat().st_size > 40960 and "thumbnail" not in f.name.lower()
        ]
        discarded = len(all_files) - len(files)
        if discarded > 0:
            logger.info(f"Takeout: Se han descartado {discarded} posibles miniaturas automáticamente.")
    else:
        files = all_files

    if not files:
        die("No se encontraron fotos/vídeos en la ruta proporcionada (o todas fueron filtradas como miniaturas).")

    if args.dry_run:
        logger.info("Modo Dry-Run activo. Archivos que se procesarían:")
        for p in files:
            logger.info(f"- {p} | {mime_for(p)} | {capture_time(p, is_takeout=args.takeout)}")
        return 0

    mc = MovistarCloud(email, password, jsessionid, validationkey)

    try:
        mc.login()
        logger.info("Login correcto.")
        
        # Obtenemos TODOS los items de galería para detectar duplicados
        existing_items = mc.get_all_gallery_items()
        
        total_size_mb = sum(p.stat().st_size for p in files) / (1024 * 1024)
        logger.info(f"Archivos a subir: {len(files)} (Total: {total_size_mb:.2f} MB)")

        total_files = len(files)

        import concurrent.futures

        def upload_task(idx, p):
            key = (p.name, p.stat().st_size)
            progress_prefix = f"[{idx}/{total_files}]"

            if key in existing_items:
                logger.info(f"{progress_prefix} [SKIP] {p.name} (Ya existe en la Galería)")
                return "skip"

            logger.info(f"{progress_prefix} [UPLOAD] Subiendo {p.name} ...")
            try:
                result = mc.upload_to_gallery(p, is_takeout=args.takeout)
                file_id = result['id']
                logger.info(f"{progress_prefix} [OK] {p.name} subido exitosamente (ID: {file_id}).")

                if not args.no_wait:
                    mc.wait_usable(file_id)
                return "ok"
            except requests.exceptions.HTTPError as exc:
                if exc.response is not None and exc.response.status_code == 401:
                    logger.error(f"{progress_prefix} [CRÍTICO] Sesión caducada (Error 401). Abortando proceso de inmediato.")
                    import os
                    os._exit(1)
                logger.error(f"{progress_prefix} [ERROR] Falló la subida HTTP: {exc}")
                return "error"
            except Exception as exc:
                logger.error(f"{progress_prefix} [ERROR] Falló la subida de {p.name}: {exc}")
                return "error"

        ok = 0
        skipped = 0
        failed = 0

        logger.info(f"Iniciando subida con {args.workers} hilos paralelos...")
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as executor:
            futures = [executor.submit(upload_task, idx, p) for idx, p in enumerate(files, 1)]
            for future in concurrent.futures.as_completed(futures):
                res = future.result()
                if res == "ok":
                    ok += 1
                elif res == "skip":
                    skipped += 1
                else:
                    failed += 1
                logger.info(f"__STATS__:{ok}:{skipped}:{failed}")

        logger.info(f"Proceso completado. Subidos={ok}, Saltados={skipped}, Errores={failed}")
        return 1 if failed else 0

    finally:
        mc.s.close()


if __name__ == "__main__":
    raise SystemExit(main())

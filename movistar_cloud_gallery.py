#!/usr/bin/env python3

from __future__ import annotations

import argparse
import atexit
import json
import logging
import mimetypes
import os
import signal
import sqlite3
import sys
import threading
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

_GLOBAL_CACHE = None

# Manejador de señal para terminación inmediata y limpia en caso de SIGTERM / SIGINT
def handle_sigterm(signum, frame):
    try:
        logging.getLogger("MovistarGallery").info("Señal de parada recibida (SIGTERM/SIGINT). Saliendo limpiamente...")
        global _GLOBAL_CACHE
        if _GLOBAL_CACHE is not None:
            _GLOBAL_CACHE.close()
    except Exception:
        pass
    os._exit(0)

try:
    signal.signal(signal.SIGTERM, handle_sigterm)
    signal.signal(signal.SIGINT, handle_sigterm)
except Exception:
    pass

class UploadCache:
    """
    Base de datos SQLite persistente para garantizar que ninguna foto o vídeo
    se vuelva a subir por duplicado, incluso tras reiniciar Docker o detener el proceso.
    Incluye sincronización con mutex (threading.RLock), PRAGMA busy_timeout (30s),
    modo WAL con degradación a DELETE/NORMAL, creación garantizada de esquema,
    fallback multinivel y recuperación automática de tablas.
    """
    def __init__(self, db_path: Path):
        self.db_path = db_path
        self._lock = threading.RLock()
        self._conn = None
        self._init_db()

    def _create_tables(self, conn: sqlite3.Connection):
        """Crea las tablas e índices necesarios si no existen."""
        with conn:
            conn.execute("""
                CREATE TABLE IF NOT EXISTS uploaded_files (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    file_path TEXT NOT NULL,
                    file_name TEXT NOT NULL,
                    file_size INTEGER NOT NULL,
                    mtime REAL NOT NULL,
                    cloud_id INTEGER,
                    uploaded_at TEXT NOT NULL,
                    status TEXT NOT NULL,
                    UNIQUE(file_path, file_size)
                )
            """)
            conn.execute("CREATE INDEX IF NOT EXISTS idx_name_size ON uploaded_files(file_name, file_size)")
            conn.execute("CREATE INDEX IF NOT EXISTS idx_file_path ON uploaded_files(file_path)")

    def _open_connection(self, path: Path, timeout: float = 30.0) -> sqlite3.Connection:
        if str(path) != ":memory:":
            path.parent.mkdir(parents=True, exist_ok=True)
            
        conn = sqlite3.connect(
            str(path),
            timeout=timeout,
            check_same_thread=False
        )
        conn.row_factory = sqlite3.Row
        
        # Timeout de espera ante bloqueos concurrentes (30 segundos)
        busy_ms = int(timeout * 1000)
        try:
            conn.execute(f"PRAGMA busy_timeout = {busy_ms};")
        except Exception:
            pass

        # Configurar modo de journal (WAL si es soportado por el FS, DELETE como respaldo)
        if str(path) != ":memory:":
            try:
                res = conn.execute("PRAGMA journal_mode = WAL;").fetchone()
                mode = str(res[0]).upper() if res else ""
                if mode != "WAL":
                    conn.execute("PRAGMA journal_mode = DELETE;")
            except Exception:
                try:
                    conn.execute("PRAGMA journal_mode = DELETE;")
                except Exception:
                    pass

        try:
            conn.execute("PRAGMA synchronous = NORMAL;")
        except Exception:
            pass

        # CRÍTICO: Siempre asegurar que las tablas existen al abrir cualquier conexión
        self._create_tables(conn)
        return conn

    def _get_conn(self, timeout: float = 30.0) -> sqlite3.Connection:
        if self._conn is not None:
            try:
                self._conn.execute("SELECT 1;")
                return self._conn
            except Exception:
                self._close_conn()
        try:
            self._conn = self._open_connection(self.db_path, timeout=timeout)
            return self._conn
        except Exception as e:
            # Si la ruta actual falló (por ej. database is locked en bind-mount), activar fallback
            self._close_conn()
            fallback = self._activate_fallback(str(e))
            self._conn = self._open_connection(fallback, timeout=timeout)
            return self._conn

    def _close_conn(self):
        if self._conn is not None:
            try:
                try:
                    self._conn.execute("PRAGMA wal_checkpoint(PASSIVE);")
                except Exception:
                    pass
                self._conn.close()
            except Exception:
                pass
            self._conn = None

    def _activate_fallback(self, reason: str) -> Path:
        candidates = [
            Path("/app/tokens/movistar_upload_cache.sqlite"),
            Path.home() / ".movistar_upload_cache.sqlite",
            Path("/tmp/movistar_upload_cache.sqlite"),
            Path(":memory:")
        ]
        for cand in candidates:
            if cand != self.db_path:
                try:
                    if str(cand) != ":memory:":
                        cand.parent.mkdir(parents=True, exist_ok=True)
                    self.db_path = cand
                    logging.getLogger("MovistarGallery").warning(
                        f"Activando ruta de caché SQLite de respaldo: {cand} (Motivo: {reason})"
                    )
                    return cand
                except Exception:
                    continue
        self.db_path = Path(":memory:")
        return self.db_path

    def _init_db(self):
        with self._lock:
            attempts = 3
            for attempt in range(attempts):
                try:
                    conn = self._open_connection(self.db_path, timeout=5.0)
                    self._create_tables(conn)
                    conn.close()
                    logging.getLogger("MovistarGallery").info(f"Caché local persistente inicializada en: {self.db_path}")
                    return
                except (sqlite3.OperationalError, sqlite3.DatabaseError) as e:
                    logging.getLogger("MovistarGallery").warning(
                        f"Intento {attempt + 1}/{attempts} de inicializar caché en {self.db_path} falló ({e}). Reintentando..."
                    )
                    time.sleep(1.0)
                except Exception as e:
                    logging.getLogger("MovistarGallery").warning(f"Error inesperado inicializando caché en {self.db_path}: {e}")
                    break

            # Si falla la ruta configurada, activar ruta de respaldo
            fallback = self._activate_fallback("bloqueo persistente o error de inicialización")
            try:
                conn = self._open_connection(fallback, timeout=5.0)
                self._create_tables(conn)
                conn.close()
                logging.getLogger("MovistarGallery").info(f"Caché de respaldo inicializada con éxito en: {fallback}")
            except Exception as fb_err:
                logging.getLogger("MovistarGallery").error(f"Error inicializando respaldo {fallback}: {fb_err}")
                self.db_path = Path(":memory:")

    def is_uploaded(self, path: Path) -> bool:
        with self._lock:
            try:
                stat = path.stat()
                size = stat.st_size
                name = path.name
                rel_path = str(path)
                
                for attempt in range(3):
                    try:
                        conn = self._get_conn()
                        # 1. Chequeo por ruta completa y tamaño
                        cur = conn.execute(
                            "SELECT id FROM uploaded_files WHERE file_path = ? AND file_size = ? AND status = 'ok' LIMIT 1",
                            (rel_path, size)
                        )
                        if cur.fetchone():
                            return True
                        # 2. Chequeo por nombre de archivo y tamaño
                        cur = conn.execute(
                            "SELECT id FROM uploaded_files WHERE file_name = ? AND file_size = ? AND status = 'ok' LIMIT 1",
                            (name, size)
                        )
                        if cur.fetchone():
                            return True
                        return False
                    except sqlite3.OperationalError as e:
                        err_msg = str(e).lower()
                        if "no such table" in err_msg:
                            self._create_tables(conn)
                            continue
                        if "locked" in err_msg or "busy" in err_msg:
                            time.sleep(0.1 * (attempt + 1))
                            continue
                        raise
            except Exception as e:
                logging.getLogger("MovistarGallery").debug(f"Error consultando caché para {path.name}: {e}")
            return False

    def mark_uploaded(self, path: Path, cloud_id: int | None = None, status: str = "ok"):
        with self._lock:
            try:
                stat = path.stat()
                size = stat.st_size
                name = path.name
                mtime = stat.st_mtime
                rel_path = str(path)
                now = datetime.now(timezone.utc).isoformat()
                
                for attempt in range(3):
                    try:
                        conn = self._get_conn()
                        with conn:
                            conn.execute("""
                                INSERT INTO uploaded_files (file_path, file_name, file_size, mtime, cloud_id, uploaded_at, status)
                                VALUES (?, ?, ?, ?, ?, ?, ?)
                                ON CONFLICT(file_path, file_size) DO UPDATE SET
                                    cloud_id = excluded.cloud_id,
                                    uploaded_at = excluded.uploaded_at,
                                    status = excluded.status
                            """, (rel_path, name, size, mtime, cloud_id, now, status))
                        return
                    except sqlite3.OperationalError as e:
                        err_msg = str(e).lower()
                        if "no such table" in err_msg:
                            self._create_tables(conn)
                            continue
                        if "locked" in err_msg or "busy" in err_msg:
                            time.sleep(0.1 * (attempt + 1))
                            continue
                        raise
            except Exception as e:
                logging.getLogger("MovistarGallery").warning(f"No se pudo registrar {path.name} en la caché: {e}")

    def import_cloud_items(self, items_dict: dict):
        if not items_dict:
            return
        with self._lock:
            try:
                now = datetime.now(timezone.utc).isoformat()
                rows = []
                for (name, size), item in items_dict.items():
                    cloud_id = item.get("id") if isinstance(item, dict) else None
                    rows.append((str(name), str(name), int(size), 0.0, cloud_id, now))
                
                for attempt in range(3):
                    try:
                        conn = self._get_conn()
                        with conn:
                            conn.executemany("""
                                INSERT OR IGNORE INTO uploaded_files (file_path, file_name, file_size, mtime, cloud_id, uploaded_at, status)
                                VALUES (?, ?, ?, ?, ?, ?, 'ok')
                            """, rows)
                        return
                    except sqlite3.OperationalError as e:
                        err_msg = str(e).lower()
                        if "no such table" in err_msg:
                            self._create_tables(conn)
                            continue
                        if "locked" in err_msg or "busy" in err_msg:
                            time.sleep(0.2 * (attempt + 1))
                            continue
                        raise
            except Exception as e:
                logging.getLogger("MovistarGallery").debug(f"Error importando items de nube a la caché: {e}")

    def close(self):
        with self._lock:
            self._close_conn()

def get_cache_db_path(target_path: Path | None = None, phone: str | None = None) -> Path:
    """
    Determina la ruta persistente para la caché SQLite, aislada por número móvil si se proporciona.
    Prioridad:
    1. MOVISTAR_CACHE_PATH de entorno si está definido
    2. /app/tokens/cache_{phone}.sqlite (volumen ext4 dedicado de Docker)
    3. ~/.cache_{phone}.sqlite (directorio home del sistema)
    4. target_path o /data
    """
    phone_clean = "".join(filter(str.isdigit, str(phone or "")))
    db_name = f"cache_{phone_clean}.sqlite" if phone_clean else "movistar_upload_cache.sqlite"

    custom = os.getenv("MOVISTAR_CACHE_PATH")
    if custom:
        return Path(custom).expanduser()

    # 1. En Docker, /app/tokens es el volumen ext4 nativo persistente
    tokens_dir = Path("/app/tokens")
    try:
        tokens_dir.mkdir(parents=True, exist_ok=True)
        if os.access(tokens_dir, os.W_OK):
            target_db = tokens_dir / db_name
            return target_db
    except Exception:
        pass

    # 2. Directorio home del usuario
    home_db = Path.home() / db_name
    try:
        home_db.parent.mkdir(parents=True, exist_ok=True)
        if os.access(home_db.parent, os.W_OK):
            return home_db
    except Exception:
        pass

    # 3. Fallback a target_path
    if target_path and target_path.is_dir() and os.access(target_path, os.W_OK):
        return target_path / f".{db_name}"
    return Path.cwd() / db_name

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

    def validate_session(self) -> bool:
        """Verifica activamente contra la API de Movistar Cloud que las credenciales/cookies son válidas."""
        url = self._vk_url(f"{BASE_URL}/sapi/system/information?action=get")
        try:
            r = self.s.get(url, timeout=15)
            if r.status_code == 200:
                return True
            elif r.status_code == 401:
                logger.error("Sesión inválida o caducada en Movistar Cloud (Error 401 Unauthorized).")
                return False
            else:
                logger.warning(f"Respuesta inesperada al validar sesión (HTTP {r.status_code}). Continuando...")
                return True
        except Exception as e:
            logger.warning(f"Aviso al validar sesión con el servidor ({e}). Continuando...")
            return True

    def login(self) -> None:
        """Valida la sesión o autentica mediante email/password."""
        if self.s.cookies.get("JSESSIONID") and self.validation_key:
            logger.info("Comprobando credenciales y estado de sesión en Movistar Cloud...")
            if not self.validate_session():
                die("La sesión de Movistar Cloud ha caducado o las credenciales no son válidas. Por favor, renueva el inicio de sesión desde el panel web.")
            logger.info("¡Sesión en Movistar Cloud verificada con éxito! Conexión lista.")
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

    def start_keepalive(self, interval_seconds: int = 300):
        """Inicia un hilo en segundo plano que envía un heartbeat periódico a Movistar Cloud para evitar que JSESSIONID caduque."""
        if hasattr(self, "_keepalive_thread") and self._keepalive_thread is not None and self._keepalive_thread.is_alive():
            return
        self._stop_keepalive = threading.Event()
        self._keepalive_thread = threading.Thread(target=self._keepalive_loop, args=(interval_seconds,), daemon=True)
        self._keepalive_thread.start()

    def _keepalive_loop(self, interval_seconds: int):
        logger.debug(f"Keep-alive activo (heartbeat cada {interval_seconds}s).")
        while not self._stop_keepalive.wait(interval_seconds):
            try:
                url = self._vk_url(f"{BASE_URL}/sapi/system/information?action=get")
                r = self.s.get(url, timeout=15)
                if r.status_code == 200:
                    logger.debug("Keep-alive heartbeat OK (sesión renovada).")
                elif r.status_code == 401:
                    logger.warning("Keep-alive detectó sesión expirada en el servidor (401).")
            except Exception as e:
                logger.debug(f"Keep-alive ping error transitorio: {e}")

    def stop_keepalive(self):
        if hasattr(self, "_stop_keepalive"):
            self._stop_keepalive.set()

    def get_all_gallery_items(self) -> dict:
        """
        Obtiene el inventario completo de la Galería de Movistar Cloud para evitar duplicados.
        Utiliza el endpoint oficial de línea de tiempo (/sapi/media/timeline?action=get)
        y consulta de metadatos en bloques por IDs, con fallback directo a /sapi/media?action=get.
        """
        logger.info("Obteniendo inventario de la Galería en Movistar Cloud para control de duplicados...")
        existing_items = {}

        # Estrategia 1: Timeline oficial (/sapi/media/timeline?action=get)
        try:
            timeline_url = self._vk_url(f"{BASE_URL}/sapi/media/timeline?action=get")
            payload = {
                "data": {
                    "granularity": "daily",
                    "sortorder": "creationdate"
                }
            }
            r = self.s.post(timeline_url, json=payload, timeout=self.timeout)
            if r.status_code == 200:
                data_obj = r.json().get("data", {})
                periods = data_obj.get("periods") or []
                all_ids = []
                for p in periods:
                    if isinstance(p, dict) and "ids" in p:
                        all_ids.extend(p["ids"])
                
                if all_ids:
                    logger.info(f"Timeline reporta {len(all_ids)} elementos en la cuenta. Cargando metadatos en bloques...")
                    chunk_size = 400
                    for i in range(0, len(all_ids), chunk_size):
                        chunk = all_ids[i:i + chunk_size]
                        items_url = self._vk_url(f"{BASE_URL}/sapi/media?action=get")
                        items_payload = {
                            "data": {
                                "ids": chunk,
                                "fields": ["name", "size", "folderid", "creationdate"]
                            }
                        }
                        try:
                            ir = self.s.post(items_url, json=items_payload, timeout=self.timeout)
                            if ir.status_code == 200:
                                media_list = ir.json().get("data", {}).get("media") or []
                                for item in media_list:
                                    name = item.get("name") or item.get("filename")
                                    size = item.get("size") or item.get("filesize") or item.get("bytes")
                                    if name is not None and size is not None:
                                        existing_items[(name, int(size))] = item
                                        existing_items[(name.lower(), int(size))] = item
                        except Exception as chunk_err:
                            logger.debug(f"Aviso consultando bloque de metadatos: {chunk_err}")

                    logger.info(f"Inventario en la nube cargado vía Timeline: {len(existing_items) // 2} elementos únicos.")
                    return existing_items
        except Exception as e:
            logger.debug(f"Estrategia Timeline no disponible ({e}), probando alternativas...")

        # Estrategia 2: Endpoint unificado /sapi/media?action=get
        try:
            url = self._vk_url(f"{BASE_URL}/sapi/media?action=get&limit=5000")
            payload = {
                "data": {
                    "fields": ["name", "size", "folder", "origin"]
                }
            }
            r = None
            try:
                r = self.s.post(url, json=payload, timeout=self.timeout)
            except requests.exceptions.HTTPError as he:
                if he.response is not None and he.response.status_code == 405:
                    r = self.s.get(url, timeout=self.timeout)
                else:
                    raise

            if r is not None and r.status_code == 405:
                r = self.s.get(url, timeout=self.timeout)

            if r is not None and r.status_code == 200:
                data_obj = r.json().get("data", {})
                items = data_obj.get("media") or data_obj.get("items") or data_obj.get("files") or []
                for item in items:
                    name = item.get("name") or item.get("filename")
                    size = item.get("size") or item.get("filesize") or item.get("bytes")
                    if name is not None and size is not None:
                        existing_items[(name, int(size))] = item
                        existing_items[(name.lower(), int(size))] = item

                if existing_items:
                    logger.info(f"Inventario en la nube cargado vía sapi/media: {len(existing_items) // 2} elementos únicos.")
                    return existing_items
        except Exception as e:
            logger.debug(f"Estrategia unificada no disponible: {e}")

        # Estrategia 3: Endpoints específicos con soporte GET y POST
        for media_type in ["picture", "video"]:
            try:
                url = self._vk_url(f"{BASE_URL}/sapi/media/{media_type}?action=get&limit=5000")
                r = self.s.get(url, timeout=self.timeout)
                if r.status_code == 405:
                    r = self.s.post(url, json={"data": {"fields": ["name", "size"]}}, timeout=self.timeout)
                
                if r.status_code == 200:
                    data_obj = r.json().get("data", {})
                    key = "pictures" if media_type == "picture" else "videos"
                    items = []
                    for k in [key, media_type, "items", "elements", "media", "files"]:
                        if k in data_obj and isinstance(data_obj[k], list):
                            items = data_obj[k]
                            break
                    for item in items:
                        name = item.get("name") or item.get("filename")
                        size = item.get("size") or item.get("filesize") or item.get("bytes")
                        if name is not None and size is not None:
                            existing_items[(name, int(size))] = item
                            existing_items[(name.lower(), int(size))] = item
            except Exception as e:
                logger.debug(f"Aviso consultando inventario de {media_type}: {e}")

        if existing_items:
            logger.info(f"Inventario en la nube: {len(existing_items) // 2} elementos detectados.")
        else:
            logger.info("Inventario en la nube no devolvió elementos previos (se continuará usando la base de datos local SQLite persistente).")
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
    parser.add_argument("--phone", type=str, default=None, help="Número de teléfono móvil asociado a la cuenta")
    args = parser.parse_args()

    # Configuración de Logging
    log_level = logging.DEBUG if args.debug else logging.INFO
    logging.basicConfig(level=log_level, format="%(asctime)s [%(levelname)s] %(message)s", datefmt="%H:%M:%S")

    phone_clean = "".join(filter(str.isdigit, str(args.phone or os.getenv("MOVISTAR_PHONE") or "")))
    if phone_clean:
        logger.info(f"Cuenta de usuario activa: {phone_clean}")
        env_paths = [
            Path(f"/app/tokens/account_{phone_clean}.env"),
            Path(f"/app/tokens/{phone_clean}.env"),
            Path(f"/app/.env_{phone_clean}"),
            Path("/app/tokens/.env"),
            Path(__file__).parent / ".env",
            Path.cwd() / ".env",
            Path.home() / ".env"
        ]
    else:
        env_paths = [Path("/app/tokens/.env"), Path(__file__).parent / ".env", Path.cwd() / ".env", Path.home() / ".env"]

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

    # 1. Validar sesión y conectar con Movistar Cloud de inmediato
    mc = MovistarCloud(email, password, jsessionid, validationkey)

    try:
        mc.login()
        mc.start_keepalive(interval_seconds=300)
        
        # 2. Inicializar base de datos de caché persistente (en volumen seguro, aislada por teléfono)
        cache_path = get_cache_db_path(path, phone=phone_clean)
        logger.info(f"Base de datos de caché local: {cache_path.name}")
        cache = UploadCache(cache_path)
        global _GLOBAL_CACHE
        _GLOBAL_CACHE = cache
        atexit.register(lambda: _GLOBAL_CACHE.close() if _GLOBAL_CACHE else None)

        # 3. Obtenemos inventario de galería en la nube para control de duplicados
        existing_items = mc.get_all_gallery_items()
        cache.import_cloud_items(existing_items)

        # 4. Explorar archivos multimedia locales con reporte periódico
        logger.info(f"Explorando archivos multimedia en {path} (recursivo={args.recursive})...")
        sys.stdout.flush()
        all_files = []
        for count, p in enumerate(iter_media_files(path, args.recursive), 1):
            all_files.append(p)
            if count % 10000 == 0:
                logger.info(f"Escaneados {count} archivos multimedia...")
                sys.stdout.flush()
        
        # Filtrado especial Takeout
        if args.takeout:
            logger.info("Modo Takeout activado: Filtrando miniaturas y leyendo .json")
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

        total_size_mb = sum(p.stat().st_size for p in files) / (1024 * 1024)
        logger.info(f"Archivos a subir: {len(files)} (Total: {total_size_mb:.2f} MB)")

        total_files = len(files)

        import concurrent.futures

        def upload_task(idx, p):
            key = (p.name, p.stat().st_size)
            key_lower = (p.name.lower(), p.stat().st_size)
            progress_prefix = f"[{idx}/{total_files}]"

            # 1. Chequeo en base de datos local SQLite persistente
            if cache.is_uploaded(p):
                logger.info(f"{progress_prefix} [SKIP] {p.name} (Ya registrado en caché persistente)")
                return "skip"

            # 2. Chequeo en inventario de Movistar Cloud
            if key in existing_items or key_lower in existing_items:
                logger.info(f"{progress_prefix} [SKIP] {p.name} (Ya existe en la Galería de Movistar Cloud)")
                cache.mark_uploaded(p, status="ok")
                return "skip"

            logger.info(f"{progress_prefix} [UPLOAD] Subiendo {p.name} ...")
            try:
                result = mc.upload_to_gallery(p, is_takeout=args.takeout)
                file_id = result.get('id')
                logger.info(f"{progress_prefix} [OK] {p.name} subido exitosamente (ID: {file_id}).")
                cache.mark_uploaded(p, cloud_id=file_id, status="ok")

                if not args.no_wait and file_id:
                    mc.wait_usable(file_id)
                return "ok"
            except requests.exceptions.HTTPError as exc:
                if exc.response is not None:
                    if exc.response.status_code == 401:
                        logger.error(f"{progress_prefix} [CRÍTICO] Sesión caducada (Error 401). Abortando proceso de inmediato.")
                        os._exit(1)
                    elif exc.response.status_code == 405:
                        logger.error(f"{progress_prefix} [ERROR 405] La API de Movistar rechazó la petición concurrente. Esperando 1s...")
                        time.sleep(1)
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
                done_count = ok + skipped + failed
                logger.info(f"__PROGRESS__{done_count}/{total_files}")
                logger.info(f"__STATS__:OK:{ok}|SKIP:{skipped}|ERR:{failed}")
                sys.stdout.flush()

        logger.info(f"Proceso completado. Subidos={ok}, Saltados={skipped}, Errores={failed}")
        return 1 if failed else 0

    finally:
        if 'mc' in locals() and mc is not None:
            mc.stop_keepalive()
            mc.s.close()
        if 'cache' in locals() and cache is not None:
            cache.close()


if __name__ == "__main__":
    raise SystemExit(main())

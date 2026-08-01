from __future__ import annotations

import base64
import binascii
import io
import re
import threading
from functools import lru_cache
from typing import Any, Literal

import numpy as np
from fastapi import FastAPI, HTTPException, Request
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse
from PIL import Image, ImageEnhance, ImageOps, UnidentifiedImageError
from pydantic import BaseModel, ConfigDict, Field, field_validator
from starlette.exceptions import HTTPException as StarletteHTTPException

app = FastAPI(title="Arena OCR Service", version="0.1.0")

_engine_lock = threading.Lock()
_request_lock = threading.BoundedSemaphore(value=1)

MAX_IMAGE_DIMENSION = 8192
MAX_IMAGE_PIXELS = 4096 * 2160
MAX_ENCODED_IMAGE_BYTES = 24 << 20
MAX_BASE64_IMAGE_CHARS = ((MAX_ENCODED_IMAGE_BYTES + 2) // 3) * 4
MAX_OCR_CROP_PIXELS = 2 << 20
MAX_REGIONS = 64
Image.MAX_IMAGE_PIXELS = MAX_IMAGE_PIXELS


@app.exception_handler(RequestValidationError)
async def request_validation_error(
    _request: Request,
    error: RequestValidationError,
) -> JSONResponse:
    method_ctx = "ocr.request_validation_error"
    fields = sorted(
        {
            ".".join(str(part) for part in item.get("loc", ()))
            for item in error.errors()
            if item.get("loc")
        },
    )
    detail = f"{method_ctx}: запрос не прошёл проверку"
    if fields:
        detail += f": некорректные поля {', '.join(fields)}"
    return JSONResponse(status_code=422, content={"detail": detail})


@app.exception_handler(StarletteHTTPException)
async def http_error(
    _request: Request,
    error: StarletteHTTPException,
) -> JSONResponse:
    method_ctx = "ocr.http_error"
    if error.status_code == 404:
        detail = f"{method_ctx}: запрошенный ресурс не найден"
    elif error.status_code == 405:
        detail = f"{method_ctx}: метод HTTP не поддерживается"
    elif isinstance(error.detail, str) and re.search(r"[А-Яа-яЁё]", error.detail):
        detail = error.detail
    else:
        detail = f"{method_ctx}: запрос завершился ошибкой HTTP {error.status_code}"
    return JSONResponse(
        status_code=error.status_code,
        content={"detail": detail},
        headers=error.headers,
    )


@app.exception_handler(Exception)
async def unhandled_error(
    _request: Request,
    _error: Exception,
) -> JSONResponse:
    method_ctx = "ocr.unhandled_error"
    return JSONResponse(
        status_code=500,
        content={"detail": f"{method_ctx}: внутренняя ошибка сервиса OCR"},
    )


class Rectangle(BaseModel):
    model_config = ConfigDict(extra="forbid")

    x: float = Field(ge=0, le=1)
    y: float = Field(ge=0, le=1)
    width: float = Field(gt=0, le=1)
    height: float = Field(gt=0, le=1)

    @field_validator("height")
    @classmethod
    def rectangle_must_fit(cls, value: float, info: Any) -> float:
        # Cross-field bounds are checked after all request fields are decoded.
        return value


class OCRRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")

    frame_id: int = Field(ge=1)
    encoding: Literal["png"]
    image: str = Field(min_length=1, max_length=MAX_BASE64_IMAGE_CHARS)
    regions: dict[str, Rectangle] = Field(min_length=1, max_length=MAX_REGIONS)


class OCRValue(BaseModel):
    raw: str
    normalized: str
    source: str = "OCR"
    confidence: float = Field(ge=0, le=1)
    region: Rectangle


class OCRResponse(BaseModel):
    values: dict[str, OCRValue]


@lru_cache(maxsize=1)
def get_engine() -> Any:
    from rapidocr import RapidOCR

    return RapidOCR()


@app.get("/healthz")
def health() -> dict[str, str]:
    # Loading ONNX models is intentionally lazy; readiness only checks the
    # process. The first OCR call warms the engine.
    return {"status": "ok"}


@app.post("/v1/ocr", response_model=OCRResponse)
def recognize(request: OCRRequest) -> OCRResponse:
    method_ctx = "ocr.recognize"
    if not _request_lock.acquire(blocking=False):
        raise HTTPException(
            status_code=429,
            detail=f"{method_ctx}: OCR уже обрабатывает другой кадр",
        )
    try:
        image = decode_image(request.image)
        values: dict[str, OCRValue] = {}
        for name, region in request.regions.items():
            validate_region(name, region)
            crop = prepare_crop(image, region)
            try:
                with _engine_lock:
                    result = get_engine()(np.asarray(crop))
                raw, confidence = extract_text(result)
            except HTTPException:
                raise
            except Exception as error:
                raise HTTPException(
                    status_code=503,
                    detail=(
                        f"{method_ctx}: не удалось распознать область "
                        f"{name!r}"
                    ),
                ) from error
            values[name] = OCRValue(
                raw=raw,
                normalized=normalize_value(raw),
                confidence=confidence,
                region=region,
            )
        return OCRResponse(values=values)
    finally:
        _request_lock.release()


def decode_image(encoded: str) -> Image.Image:
    method_ctx = "ocr.decode_image"
    if len(encoded) > MAX_BASE64_IMAGE_CHARS:
        raise HTTPException(
            status_code=413,
            detail=f"{method_ctx}: изображение превышает допустимый размер",
        )
    try:
        payload = base64.b64decode(encoded, validate=True)
        if len(payload) > MAX_ENCODED_IMAGE_BYTES:
            raise HTTPException(
                status_code=413,
                detail=f"{method_ctx}: PNG превышает предел {MAX_ENCODED_IMAGE_BYTES} байт",
            )
        image = Image.open(io.BytesIO(payload))
        validate_image_dimensions(image.width, image.height)
        image.load()
    except HTTPException:
        raise
    except (binascii.Error, UnidentifiedImageError, OSError, ValueError) as error:
        raise HTTPException(
            status_code=400,
            detail=f"{method_ctx}: передано некорректно закодированное изображение",
        ) from error
    return image.convert("RGB")


def validate_image_dimensions(width: int, height: int) -> None:
    method_ctx = "ocr.validate_image_dimensions"
    if width <= 0 or height <= 0:
        raise HTTPException(
            status_code=400,
            detail=f"{method_ctx}: передано пустое изображение",
        )
    if (
        width > MAX_IMAGE_DIMENSION
        or height > MAX_IMAGE_DIMENSION
        or width * height > MAX_IMAGE_PIXELS
    ):
        raise HTTPException(
            status_code=413,
            detail=(
                f"{method_ctx}: размер изображения {width}x{height} "
                f"превышает безопасный предел"
            ),
        )


def validate_region(name: str, region: Rectangle) -> None:
    method_ctx = "ocr.validate_region"
    if region.x + region.width > 1 or region.y + region.height > 1:
        raise HTTPException(
            status_code=422,
            detail=f"{method_ctx}: область {name!r} выходит за границы изображения",
        )


def prepare_crop(image: Image.Image, region: Rectangle) -> Image.Image:
    method_ctx = "ocr.prepare_crop"
    left = max(0, round(region.x * image.width))
    top = max(0, round(region.y * image.height))
    right = min(image.width, round((region.x + region.width) * image.width))
    bottom = min(image.height, round((region.y + region.height) * image.height))
    if right <= left or bottom <= top:
        raise HTTPException(
            status_code=422,
            detail=f"{method_ctx}: область преобразуется в пустой фрагмент",
        )
    crop_width = right - left
    crop_height = bottom - top
    scale = max(1, min(4, round(64 / max(crop_height, 1))))
    target_width = crop_width * scale
    target_height = crop_height * scale
    if (
        target_width > MAX_IMAGE_DIMENSION
        or target_height > MAX_IMAGE_DIMENSION
        or target_width * target_height > MAX_OCR_CROP_PIXELS
    ):
        raise HTTPException(
            status_code=413,
            detail=f"{method_ctx}: подготовленный фрагмент превышает безопасный предел",
        )
    crop = image.crop((left, top, right, bottom))
    if scale > 1:
        crop = crop.resize((target_width, target_height), Image.Resampling.LANCZOS)
    crop = ImageOps.autocontrast(crop)
    return ImageEnhance.Sharpness(crop).enhance(1.4)


def extract_text(result: Any) -> tuple[str, float]:
    txts = getattr(result, "txts", None)
    scores = getattr(result, "scores", None)
    if txts is not None:
        texts = [str(value).strip() for value in txts if str(value).strip()]
        numeric_scores = [float(value) for value in (scores or [])]
        return join_text(texts), mean_confidence(numeric_scores)

    # Compatibility with RapidOCR 1.x/2.x tuple output:
    # ([box, text, score], ...), elapsed.
    rows = result[0] if isinstance(result, tuple) and result else result
    texts: list[str] = []
    numeric_scores: list[float] = []
    if isinstance(rows, (list, tuple)):
        for row in rows:
            if not isinstance(row, (list, tuple)) or len(row) < 3:
                continue
            text = str(row[1]).strip()
            if text:
                texts.append(text)
                numeric_scores.append(float(row[2]))
    return join_text(texts), mean_confidence(numeric_scores)


def join_text(values: list[str]) -> str:
    return " ".join(values).strip()


def mean_confidence(scores: list[float]) -> float:
    if not scores:
        return 0.0
    return max(0.0, min(1.0, sum(scores) / len(scores)))


def normalize_value(value: str) -> str:
    value = value.strip()
    if not value:
        return ""
    compact = re.sub(r"[\s\u00a0\u202f,_]", "", value)
    numeric = re.fullmatch(r"[-+]?\d+(?:[.]\d+)?", compact)
    if numeric:
        return compact
    return re.sub(r"\s+", " ", value)

import base64
import io

import pytest
from fastapi import HTTPException
from fastapi.testclient import TestClient
from PIL import Image

from app.main import (
    MAX_BASE64_IMAGE_CHARS,
    Rectangle,
    app,
    decode_image,
    normalize_value,
    prepare_crop,
    validate_image_dimensions,
)


def _raise_unhandled_error() -> None:
    raise RuntimeError("internal server error")


app.add_api_route(
    "/__tests__/unhandled-error",
    _raise_unhandled_error,
    methods=["GET"],
)


def test_normalize_monetary_value() -> None:
    assert normalize_value("18\u202f430") == "18430"
    assert normalize_value("  item   name ") == "item name"


def test_decode_and_crop() -> None:
    image = Image.new("RGB", (100, 50), "white")
    payload = io.BytesIO()
    image.save(payload, "PNG")
    decoded = decode_image(base64.b64encode(payload.getvalue()).decode())
    crop = prepare_crop(
        decoded,
        Rectangle(x=0.1, y=0.2, width=0.5, height=0.4),
    )
    assert crop.width >= 50
    assert crop.height >= 20


def test_decode_rejects_oversized_payload_before_allocation() -> None:
    with pytest.raises(HTTPException) as error:
        decode_image("A" * (MAX_BASE64_IMAGE_CHARS + 1))
    assert error.value.status_code == 413


def test_image_dimensions_are_bounded() -> None:
    validate_image_dimensions(4096, 2160)
    with pytest.raises(HTTPException) as error:
        validate_image_dimensions(4096, 2161)
    assert error.value.status_code == 413


def test_prepare_crop_rejects_full_4k_ocr() -> None:
    class ImageShape:
        width = 4096
        height = 2160

    with pytest.raises(HTTPException) as error:
        prepare_crop(ImageShape(), Rectangle(x=0, y=0, width=1, height=1))  # type: ignore[arg-type]
    assert error.value.status_code == 413


def test_http_routing_errors_are_contextual_and_russian() -> None:
    client = TestClient(app, raise_server_exceptions=False)

    not_found = client.get("/неизвестно")
    assert not_found.status_code == 404
    assert not_found.json() == {
        "detail": "ocr.http_error: запрошенный ресурс не найден",
    }

    method_not_allowed = client.post("/healthz")
    assert method_not_allowed.status_code == 405
    assert method_not_allowed.json() == {
        "detail": "ocr.http_error: метод HTTP не поддерживается",
    }


def test_unhandled_error_is_contextual_and_russian() -> None:
    client = TestClient(app, raise_server_exceptions=False)

    response = client.get("/__tests__/unhandled-error")

    assert response.status_code == 500
    assert response.json() == {
        "detail": "ocr.unhandled_error: внутренняя ошибка сервиса OCR",
    }

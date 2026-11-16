# syntax=docker/dockerfile:1
# AI service (FastAPI) cho backend Go: parse-cv, analyze-cv, score.
# Build từ thư mục gốc: docker compose build ai

FROM python:3.13-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    HF_HOME=/models

WORKDIR /app

# torch bản CPU cài trước, nếu không pip sẽ kéo bản CUDA vài GB
RUN pip install torch --index-url https://download.pytorch.org/whl/cpu

COPY requirements-prod.txt ./
RUN pip install -r requirements-prod.txt

COPY api.py ./
COPY matching ./matching

RUN useradd --create-home --uid 1000 app \
    && mkdir -p /app/data /models \
    && chown -R app:app /app /models
USER app

EXPOSE 8001

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8001/health', timeout=4)"

# Chỉ 1 worker: mỗi worker nạp một bản mô hình riêng (~2GB RAM)
CMD ["uvicorn", "api:app", "--host", "0.0.0.0", "--port", "8001", "--workers", "1"]

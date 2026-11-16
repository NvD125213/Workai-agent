import time

import numpy as np
from . import config
from .embedder import setup_torch

def load_reranker():
    """Reranker theo config.RERANK_BACKEND: api (production) hoặc torch (development)."""
    return ApiReranker() if config.RERANK_BACKEND == "api" else Reranker()

def _to_unit(s: np.ndarray) -> np.ndarray:
    # Tuỳ phiên bản thư viện/dịch vụ, đầu ra có thể là logit chưa qua sigmoid -> đưa về 0..1
    if s.size and (s.min() < 0 or s.max() > 1):
        s = 1.0 / (1.0 + np.exp(-s))
    return s

class Reranker:
    def __init__(self, model_name: str = config.RERANK_MODEL):
        setup_torch()
        from sentence_transformers import CrossEncoder

        self.model_name = model_name
        self.model = CrossEncoder(model_name, max_length=config.RERANK_MAX_LEN, device="cpu")

    def score(self, query: str, docs) -> np.ndarray:
        if not docs:
            return np.zeros(0)
        pairs = [(query, d) for d in docs]
        s = np.asarray(
            self.model.predict(pairs, batch_size=config.RERANK_BATCH, show_progress_bar=False),
            dtype=float,
        ).reshape(-1)
        return _to_unit(s)

class ApiReranker:
    """Gọi API rerank định dạng SiliconFlow/Jina/Cohere: {model, query, documents} -> results[{index, relevance_score}].

    Không tốn RAM; mỗi lượt chấm chỉ gửi các job đã qua bộ lọc cứng nên số token nhỏ.
    """

    def __init__(self, model_name: str = config.RERANK_MODEL, client=None):
        import httpx

        if not config.RERANK_API_KEY and client is None:
            raise RuntimeError("Chưa đặt RERANK_API_KEY trong .env cho RERANK_BACKEND=api")
        self.model_name = model_name
        self.client = client or httpx.Client(
            timeout=config.RERANK_API_TIMEOUT, headers={"Authorization": f"Bearer {config.RERANK_API_KEY}"})

    def _post(self, payload: dict) -> dict:
        import httpx

        for attempt in range(config.RERANK_API_RETRIES + 1):
            try:
                resp = self.client.post(config.RERANK_API_URL, json=payload)
                if resp.status_code != 429 and resp.status_code < 500:
                    resp.raise_for_status()        # 400, 401... gọi lại cũng vô ích
                    return resp.json()
                error = RuntimeError(f"rerank API trả về {resp.status_code}: {resp.text[:200]}")
            except httpx.TransportError as e:      # hết thời gian chờ, mất kết nối
                error = e
            if attempt < config.RERANK_API_RETRIES:
                time.sleep(2 ** attempt)
        raise error

    def score(self, query: str, docs) -> np.ndarray:
        docs = list(docs)
        s = np.zeros(len(docs))
        for start in range(0, len(docs), config.RERANK_API_BATCH):
            batch = docs[start:start + config.RERANK_API_BATCH]
            data = self._post({"model": self.model_name, "query": query, "documents": batch,
                               "top_n": len(batch), "return_documents": False})
            results = data.get("results") or []
            if len(results) != len(batch):
                raise RuntimeError(f"rerank API trả về {len(results)} kết quả cho {len(batch)} job")
            for r in results:
                s[start + r["index"]] = float(r["relevance_score"])
        return _to_unit(s)

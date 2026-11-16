"""Test ApiReranker với API giả (không gọi mạng, không cần key).

Chạy: .venv\\Scripts\\python.exe tests\\test_reranker.py
"""
import json
import sys
from pathlib import Path

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from matching import config  # noqa: E402
from matching.matcher import Matcher  # noqa: E402
from matching.reranker import ApiReranker  # noqa: E402


def _client(handler):
    return httpx.Client(transport=httpx.MockTransport(handler))


def test_scores_follow_input_order():
    """API trả kết quả theo thứ tự điểm giảm dần; điểm phải được đặt lại đúng vị trí job."""
    sent = []

    def handler(req):
        body = json.loads(req.content)
        sent.append(body)
        scores = {"java": 0.2, "go": 0.9, "php": 0.5}
        results = sorted(({"index": i, "relevance_score": scores[d]} for i, d in enumerate(body["documents"])),
                         key=lambda r: -r["relevance_score"])
        return httpx.Response(200, json={"results": results})

    s = ApiReranker("m", client=_client(handler)).score("q", ["java", "go", "php"])
    assert s.tolist() == [0.2, 0.9, 0.5]
    assert sent[0]["model"] == "m" and sent[0]["query"] == "q" and sent[0]["top_n"] == 3


def test_batches_and_retry():
    real = config.RERANK_API_BATCH, config.RERANK_API_RETRIES
    calls = []

    def handler(req):
        calls.append(1)
        if len(calls) == 1:
            return httpx.Response(503, text="overloaded")
        docs = json.loads(req.content)["documents"]
        return httpx.Response(200, json={"results": [{"index": i, "relevance_score": 0.5} for i in range(len(docs))]})

    try:
        config.RERANK_API_BATCH, config.RERANK_API_RETRIES = 2, 1
        import matching.reranker as rr
        rr.time.sleep = lambda s: None
        s = ApiReranker("m", client=_client(handler)).score("q", ["a", "b", "c"])
        assert s.tolist() == [0.5, 0.5, 0.5]
        assert len(calls) == 3, "503 gọi lại 1 lần, sau đó 2 batch"
    finally:
        config.RERANK_API_BATCH, config.RERANK_API_RETRIES = real


def test_matcher_degrades_on_api_error():
    """API rerank lỗi: vẫn chấm điểm, rerank = None, có rerank_error."""
    real = config.RERANK_BACKEND, config.RERANK_API_RETRIES
    try:
        config.RERANK_BACKEND, config.RERANK_API_RETRIES = "api", 0
        reranker = ApiReranker("m", client=_client(lambda req: httpx.Response(401, text="bad key")))
        profile = {"skills": [{"name": "go"}], "years_experience": 3}
        jobs = [{"id": "1", "title": "Go Developer", "description": "Go", "skills": ["go"], "min_years": 1}]
        out = Matcher(embedder=object(), reranker=reranker).score_jobs(profile, jobs, use_rerank=True)
        assert out["results"][0]["components"]["rerank"] is None
        assert "401" in out["rerank_error"]
    finally:
        config.RERANK_BACKEND, config.RERANK_API_RETRIES = real


if __name__ == "__main__":
    tests = [(n, f) for n, f in list(globals().items()) if n.startswith("test_") and callable(f)]
    failed = 0
    for name, fn in tests:
        try:
            fn()
            print(f"PASS  {name}")
        except AssertionError as e:
            failed += 1
            print(f"FAIL  {name}: {e}")
    print(f"\n{len(tests) - failed}/{len(tests)} test đạt" if not failed else f"\n{failed} test lỗi")
    sys.exit(1 if failed else 0)

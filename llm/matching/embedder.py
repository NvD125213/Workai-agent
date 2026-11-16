import numpy as np
from . import config

def setup_torch():
    """Giới hạn số luồng CPU cho PyTorch nếu được cấu hình."""
    if config.CPU_THREADS:
        import torch

        torch.set_num_threads(config.CPU_THREADS)

def load_embedder():
    """Embedder theo config.EMBED_BACKEND: onnx (production, RAM thấp) hoặc torch (development)."""
    return OnnxEmbedder() if config.EMBED_BACKEND == "onnx" else Embedder()

class _BaseEmbedder:
    model_name = ""
    is_e5 = False

    def _encode(self, texts, show_progress=False) -> np.ndarray:
        raise NotImplementedError

    def _check_dim(self, dim: int):
        if dim != config.EMBED_DIM:
            raise RuntimeError(
                f"Mô hình {self.model_name} cho vector {dim} chiều, nhưng EMBED_DIM = {config.EMBED_DIM}. "
                "Hãy sửa EMBED_DIM và cột vector(...) trong DB cho khớp."
            )

    def encode_docs(self, texts, show_progress=False) -> np.ndarray:
        """Dùng cho văn bản được lưu và tìm kiếm (job, hồ sơ)."""
        if self.is_e5:
            texts = ["passage: " + t for t in texts]
        return self._encode(texts, show_progress)

    def encode_query(self, text: str) -> np.ndarray:
        """Dùng cho văn bản truy vấn (ví dụ công việc mong muốn)."""
        if self.is_e5:
            text = "query: " + text
        return self._encode([text])[0]

class Embedder(_BaseEmbedder):
    """sentence-transformers trên PyTorch, fp32."""

    def __init__(self, model_name: str = config.EMBED_MODEL):
        setup_torch()
        from sentence_transformers import SentenceTransformer

        self.model_name = model_name
        self.model = SentenceTransformer(model_name, device="cpu")
        self.model.max_seq_length = config.EMBED_MAX_SEQ
        self.is_e5 = "e5" in model_name.lower()   # họ e5 cần tiền tố "query: "/"passage: "

        # sentence-transformers mới đổi tên hàm, bản cũ chỉ có get_sentence_embedding_dimension
        get_dim = getattr(self.model, "get_embedding_dimension", None) or self.model.get_sentence_embedding_dimension
        self._check_dim(get_dim())

    def _encode(self, texts, show_progress=False) -> np.ndarray:
        emb = self.model.encode(
            list(texts),
            batch_size=config.EMBED_BATCH,
            normalize_embeddings=True,   # đã chuẩn hoá -> tích vô hướng chính là cosine
            show_progress_bar=show_progress,
            convert_to_numpy=True,
        )
        return emb.astype("float32")

class OnnxEmbedder(_BaseEmbedder):
    """bge-m3 bản ONNX lượng tử hoá int8 chạy bằng onnxruntime, không cần PyTorch.

    RAM ~0.9GB thay vì ~2GB, nhanh hơn bản PyTorch fp32 khoảng 2 lần trên CPU; cosine lệch bản fp32
    trung bình ~0.01 (đo trên 50 job TopCV, tương quan thứ hạng 0.99). Chỉ dùng cho bge-m3 (pooling CLS).
    """

    def __init__(self, repo: str = config.EMBED_ONNX_REPO, file: str = config.EMBED_ONNX_FILE):
        import onnxruntime as ort
        from huggingface_hub import hf_hub_download
        from tokenizers import Tokenizer

        self.model_name = config.EMBED_MODEL
        self.tokenizer = Tokenizer.from_file(hf_hub_download(repo, "tokenizer.json"))
        self.tokenizer.enable_truncation(config.EMBED_MAX_SEQ)
        self.tokenizer.enable_padding()

        opts = ort.SessionOptions()
        opts.enable_cpu_mem_arena = False      # trả lại RAM sau mỗi lần chạy thay vì giữ cho lần sau
        opts.inter_op_num_threads = 1
        if config.CPU_THREADS:
            opts.intra_op_num_threads = config.CPU_THREADS
        self.session = ort.InferenceSession(hf_hub_download(repo, file), opts, providers=["CPUExecutionProvider"])
        self._check_dim(self._encode(["a"]).shape[1])

    def _encode(self, texts, show_progress=False) -> np.ndarray:
        texts = list(texts)
        out = np.zeros((len(texts), 0), dtype="float32")
        # Xếp theo độ dài để mỗi batch ít phải đệm (padding), xong trả lại thứ tự ban đầu
        order = sorted(range(len(texts)), key=lambda i: -len(texts[i]))
        parts = []
        for i in range(0, len(order), config.EMBED_BATCH):
            enc = self.tokenizer.encode_batch([texts[k] for k in order[i:i + config.EMBED_BATCH]])
            hidden = self.session.run(None, {
                "input_ids": np.array([e.ids for e in enc], dtype=np.int64),
                "attention_mask": np.array([e.attention_mask for e in enc], dtype=np.int64),
            })[0]
            cls = hidden[:, 0]
            parts.append(cls / np.linalg.norm(cls, axis=1, keepdims=True))
        if parts:
            out = np.empty((len(texts), parts[0].shape[1]), dtype="float32")
            out[order] = np.concatenate(parts)
        return out

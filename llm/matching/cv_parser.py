import io
import re 
from pathlib import Path
from .normalize import nfc

SUPPORTED_EXT = (".pdf", ".docx", ".txt", ".md")

def clean_text(text: str) -> str:
    text = nfc(text).replace("\x00", " ")
    text = re.sub(r"[ \t\u00a0]+", " ", text)      # gộp khoảng trắng thừa
    text = re.sub(r"\n\s*\n+", "\n\n", text)        # gộp dòng trống liên tiếp
    return text.strip()

def _read_pdf(data: bytes) -> str:
    import pymupdf

    with pymupdf.open(stream=data, filetype="pdf") as doc:
        # sort=True: đọc theo thứ tự trên xuống, trái sang phải (CV hay chia nhiều cột)
        return "\n".join(page.get_text("text", sort=True) for page in doc)

_MC_FALLBACK = "{http://schemas.openxmlformats.org/markup-compatibility/2006}Fallback"


def _read_docx(data: bytes) -> str:
    """Đọc mọi đoạn văn theo đúng thứ tự trong file.

    CV thường trình bày bằng bảng chia cột hoặc text box, nên không dùng doc.paragraphs
    (bỏ sót text box) mà duyệt cả cây XML: đoạn văn thường, trong bảng, bảng lồng, text box.
    """
    from docx import Document
    from docx.oxml.ns import qn

    w_p, w_t, w_tab, w_br = qn("w:p"), qn("w:t"), qn("w:tab"), qn("w:br")
    body = Document(io.BytesIO(data)).element.body
    paragraphs = {}                                  # đoạn văn -> nội dung, giữ thứ tự xuất hiện
    for el in body.iter(w_t, w_tab, w_br):
        para, node = None, el.getparent()
        while node is not None:
            if node.tag == _MC_FALLBACK:             # bản sao dự phòng của text box, bỏ để không trùng
                break
            if para is None and node.tag == w_p:
                para = node
            node = node.getparent()
        if node is not None or para is None:
            continue
        piece = (el.text or "") if el.tag == w_t else ("\t" if el.tag == w_tab else "\n")
        paragraphs[para] = paragraphs.get(para, "") + piece
    return "\n".join(paragraphs.values())

def read_cv_bytes(data: bytes, filename: str) -> str:
    ext = Path(filename).suffix.lower()
    if ext == ".pdf":
        text = _read_pdf(data)
    elif ext == ".docx":
        text = _read_docx(data)
    elif ext in (".txt", ".md"):
        text = data.decode("utf-8-sig", errors="ignore")  # -sig: bỏ BOM do Notepad thêm vào
    elif ext == ".doc":
        raise ValueError("File .doc (Word đời cũ) chưa được hỗ trợ. Hãy lưu lại thành .docx hoặc PDF.")
    else:
        raise ValueError(f"Định dạng {ext} chưa được hỗ trợ. Hãy dùng: {', '.join(SUPPORTED_EXT)}")

    text = clean_text(text)
    if len(text) < 50:
        raise ValueError("Không đọc được chữ trong CV. Có thể CV là ảnh scan, cần bản PDF có chữ chọn được.")
    return text


def read_cv_file(path) -> str:
    p = Path(path)
    return read_cv_bytes(p.read_bytes(), p.name)
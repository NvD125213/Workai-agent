"""Báo cáo markdown cho một lần chấm điểm: hồ sơ, thống kê, bảng xếp hạng, job bị loại.

Dùng cho tests\\test_matching.py (data\\reports\\matching_<cv>.md) và cho mỗi lượt find-job
của backend (data\\reports\\find_job_<id>.md, do api.py /score ghi).
"""
import statistics
from collections import Counter
from datetime import datetime
from pathlib import Path

from . import config
from .filters import required_years


def _stats(values) -> str:
    v = [x for x in values if x is not None]
    if not v:
        return "-"
    return f"min {min(v):.3f} · trung vị {statistics.median(v):.3f} · max {max(v):.3f}"


def _fmt(v) -> str:
    return "-" if v is None else f"{v:.2f}"


def _cell(text) -> str:
    return str(text).replace("|", "\\|").replace("\n", " ")


def _job_link(job) -> str:
    return f"[{_cell(job['title'])}]({job['url']})" if job.get("url") else _cell(job["title"])


def _source(job_id) -> str:
    """'topcv:2334718' -> 'topcv'; id không có nguồn -> ''."""
    return str(job_id).split(":", 1)[0] if ":" in str(job_id) else ""


def reason_kind(reason: str) -> str:
    """Gom lý do loại để đếm: số năm cụ thể của từng job không quan trọng, kỹ năng bắt buộc thì giữ."""
    if reason.startswith("cần "):
        return "không đủ kinh nghiệm"
    if reason.startswith("job cần "):
        return "ngoài khoảng kinh nghiệm phù hợp"
    if reason.startswith("đã hết hạn nộp"):
        return "đã hết hạn nộp hồ sơ"
    return reason


def _years(job) -> str:
    need = required_years(job)
    return "-" if need is None else f"{need:g}"


def _deadline(result) -> str:
    left = result.get("days_left")
    return "-" if left is None else f"{result['deadline_factor']:.2f} ({left:.1f}d)"


def _salary(job) -> str:
    """Lương gốc của tin, kèm số triệu đã quy đổi nếu khác cách ghi gốc."""
    raw, value = _cell(job.get("salary") or ""), job.get("salary_million")
    if value is None:
        return raw or "-"
    return f"{raw} (~{value:g}tr)" if raw else f"~{value:g}tr"


def reject_summary(rejected) -> list:
    """[(lý do, số job)] giảm dần; một job có thể có nhiều lý do."""
    counts = Counter(reason_kind(x) for r in rejected for x in r["reject_reasons"])
    return counts.most_common()


def write_report(path, heading: str, meta: list, profile: dict, out: dict, jobs: list,
                 timings: dict = None, cosines=None) -> Path:
    """Ghi báo cáo ra path. meta: các dòng mô tả lần chạy (nguồn dữ liệu, công việc mong muốn...)."""
    path = Path(path)
    by_id = {j["id"]: j for j in jobs}
    results, rejected = out["results"], out["rejected"]
    passed = sum(r["passed"] for r in results)
    exp_mode = any("salary" in r["components"] for r in results) or "exp_band" in out
    weights = {**config.WEIGHTS, **config.EXP_FILTER_WEIGHTS} if exp_mode else config.WEIGHTS
    lines = [
        f"# {heading}",
        "",
        f"- Thời điểm chạy: {datetime.now():%Y-%m-%d %H:%M}",
        *[f"- {m}" for m in meta],
        f"- Bộ lọc cứng loại **{len(rejected)}/{len(jobs)}** job (sai cấp kinh nghiệm, thiếu kỹ năng bắt buộc hoặc hết hạn nộp)",
        f"- Ngưỡng đạt (`MATCH_THRESHOLD`): {config.MATCH_THRESHOLD}, đạt **{passed}/{len(results)}** job còn lại",
        "- Trọng số: " + ", ".join(f"{k} {w}" for k, w in weights.items()),
    ]
    if exp_mode:
        band = out.get("exp_band")
        lines.append("- Lọc kinh nghiệm (exp): bật, giữ job yêu cầu "
                     + (f"**{band[0]:g}-{band[1]:g} năm**" if band else "mọi số năm (CV không rõ số năm)")
                     + ", xếp hạng thêm theo lương (salary) rồi kinh nghiệm (exp_fit)")
    lines += [
        "",
        "## Hồ sơ ứng viên",
        "",
        f"- Chức danh hiện tại: {profile.get('current_title') or '-'}",
        f"- Các chức danh: {', '.join(profile.get('job_titles') or []) or '-'}",
        f"- Số năm kinh nghiệm: {profile.get('years_experience')}",
        f"- Ngoại ngữ: {', '.join(profile.get('languages') or []) or '-'}",
        f"- Địa điểm: {profile.get('location') or '-'}",
        "- Kỹ năng: " + ", ".join(f"{s['name']}" + (f" ({s['level']})" if s.get("level") else "")
                                  for s in profile.get("skills", [])),
        "",
    ]

    sources = sorted({_source(j["id"]) for j in jobs} - {""})
    if len(sources) > 1:
        status = {r["job_id"]: ("passed" if r["passed"] else "scored") for r in results}
        status.update({r["job_id"]: "rejected" for r in rejected})
        lines += ["## Theo nguồn", "",
                  "| Nguồn | Đã cào | Bị loại | Được chấm | Đạt |", "|---|---:|---:|---:|---:|"]
        for src in sources:
            ids = [j["id"] for j in jobs if _source(j["id"]) == src]
            n = Counter(status.get(i) for i in ids)
            lines.append(f"| {src} | {len(ids)} | {n['rejected']} | {n['scored'] + n['passed']} | {n['passed']} |")
        lines.append("")

    if timings or cosines is not None:
        lines += ["## Thống kê", ""]
        if timings:
            lines.append("- Thời gian: " + ", ".join(f"{k} {v:.1f}s" for k, v in timings.items()))
        if cosines is not None:
            lines.append(f"- Cosine hồ sơ-job: {_stats(cosines)} "
                         f"(`SEMANTIC_LOW`/`SEMANTIC_HIGH` = {config.SEMANTIC_LOW}/{config.SEMANTIC_HIGH})")
        reranked = [r['components']['rerank'] for r in results if r['components']['rerank'] is not None]
        lines.append(f"- Rerank ({len(reranked)}/{len(results)} job có điểm sơ bộ cao nhất, "
                     f"`RERANK_TOP_N` = {config.RERANK_TOP_N}): {_stats(reranked)}")
        lines.append(f"- Title: {_stats([r['components']['title'] for r in results])}")
        lines.append("")

    exp_cols = " Salary | ExpFit |" if exp_mode else ""
    lines += [
        "## Bảng xếp hạng",
        "",
        "| # | Điểm | Đạt | Phạt | Hạn | Skill | Rerank | Title | Exp | Semantic | Extra |" + exp_cols
        + " Năm YC | Lương | Job | Kỹ năng có | Kỹ năng thiếu |",
        "|---:|---:|:---:|---:|---:|---:|---:|---:|---:|---:|---:|" + ("---:|---:|" if exp_mode else "")
        + "---:|---|---|---|---|",
    ]
    for r in results:
        c, job = r["components"], by_id[r["job_id"]]
        exp_cells = f" {_fmt(c.get('salary'))} | {_fmt(c.get('exp_fit'))} |" if exp_mode else ""
        lines.append(
            f"| {r['rank']} | {r['score']:.1f} | {'✅' if r['passed'] else ''} | {r['penalty']:.2f} | {_deadline(r)} | {_fmt(c['skills'])} | "
            f"{_fmt(c['rerank'])} | {_fmt(c['title'])} | {_fmt(c['experience'])} | {_fmt(c['semantic'])} | "
            f"{_fmt(c['extra'])} |{exp_cells} {_years(job)} | {_salary(job)} | {_job_link(job)} | "
            f"{_cell(', '.join(r['matched_skills'])) or '-'} | {_cell(', '.join(r['missing_skills'])) or '-'} |"
        )
    lines += ["",
              "Thành phần `-` là không có dữ liệu; `combine` bỏ qua và chia lại trọng số cho các thành phần còn lại. "
              "`Phạt` là hệ số nhân vào điểm khi thiếu techstack quan trọng (1 = không phạt). "
              "`Hạn` là hệ số nhân khi sắp hết hạn nộp, kèm số ngày còn lại (`-` = tin không ghi hạn).",
              ""]

    if rejected:
        lines += ["## Lý do loại phổ biến", "", "| Lý do | Số job |", "|---|---:|"]
        lines += [f"| {_cell(reason)} | {n} |" for reason, n in reject_summary(rejected)]
        lines += ["", "## Job bị bộ lọc cứng loại", "", "| Job | Lý do | Kỹ năng thiếu |", "|---|---|---|"]
        for r in rejected:
            lines.append(f"| {_job_link(by_id[r['job_id']])} | {_cell('; '.join(r['reject_reasons']))} | "
                         f"{_cell(', '.join(r['missing_skills'])) or '-'} |")
        lines.append("")

    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines), encoding="utf-8")
    return path

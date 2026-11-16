import os
import sys
from pathlib import Path

# Thêm thư mục llm\ vào sys.path để import được package matching, chạy từ thư mục nào cũng được
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
# Khi output bị chuyển hướng (nút Run của VS Code, pipe), Windows dùng cp1252 và print tiếng Việt sẽ lỗi
sys.stdout.reconfigure(encoding="utf-8")

from matching import config
from matching.normalize import (
    parse_skill_list, match_skills, detect_city, detect_languages, is_remote, nfc,
)

print("HF_HOME:", os.environ["HF_HOME"])
print("Thư mục dữ liệu:", config.DATA_DIR)

# 1) Chuẩn hoá danh sách kỹ năng
print(parse_skill_list("ReactJS; NodeJS, Postgres | k8s; Python; python"))

# 2) Ranh giới từ: CV có JavaScript nhưng KHÔNG có Java
cv = nfc("Kỹ năng: JavaScript, React.js, PostgreSQL, Docker. IELTS 6.5. Địa chỉ: Quận 7, TP.HCM").lower()
matched, missing = match_skills(["java", "javascript", "react", "postgresql", "kubernetes"], [], cv)
print("Khớp:", matched)
print("Thiếu:", missing)

# 3) Ngoại ngữ, thành phố, remote
print("Ngoại ngữ:", detect_languages(cv))
print("Thành phố:", detect_city(cv))
print("Remote?:", is_remote("Làm việc từ xa toàn thời gian"))
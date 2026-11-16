Một số điểm chưa hợp lý

1. Khi tích hợp lên dùng giao diện gửi, /analyze-cv sẽ phải chạy ngầm, trả về thông báo đang chạy, đang tiến hành thay vì treo lâu như vậy

2. Pause mỗi lần crawp hơi lâu, tăng lên một chút, tầm 15-20s thôi
3. Các crawp hoạt động đồng thời luôn để tiết kiệm thời gian
4. Pause là pause giữa các lần thực hiện gọi description, 5-10s chứ ko phải pause 1 lần luôn
5. Cải tiến POST /find-job -> find theo id, ko find full tránh gây quá tải
6. Áp dụng bộ lọc kinh nghiệm -> Thêm vào lúc POST /find-job/:id query exp boolean, exp false thì phân tích như thường, exp true thì thêm điều kiện phân tích về exp và lương, ví dụ như báo cáo tôi vừa test đang xếp 1 công việc thực tập sinh cho 1 ứng viên có 5 năm kinh nghiệm rank điểm 99,7, đó là thứ chưa hợp lý. Sau khi có kết quả cần thêm filter cải thiện thêm vấn đề sau: Ví dụ với kinh nghiệp 2 năm lọc các job từ 0-2 năm, 3 năm từ 2-3 năm, 4 hay 5 năm là từ 2-5 năm, áp dụng thêm ranking về thu nhập và exp, ví dụ với 1 job 2 năm 20m so với 1 job 3 năm 18m thì sẽ phải ranking job 2 năm 20m hơn. Mức lương (nếu có) > exp

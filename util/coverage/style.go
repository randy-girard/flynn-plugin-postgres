package main

const reportCSS = `*{box-sizing:border-box}
body{margin:0;background:#f4f6f8;color:#1c2430;font:15px/1.45 ui-sans-serif,system-ui,-apple-system,sans-serif}
main{max-width:1080px;margin:0 auto;padding:32px 24px 80px}
main.file{max-width:1200px}
h1{margin:0 0 6px;font-size:28px;font-weight:650}
h2{margin:0 0 8px;font-size:20px}
.kicker{margin:0 0 8px;color:#5b6777;font-size:13px;letter-spacing:.04em;text-transform:uppercase}
.hero{display:flex;justify-content:space-between;align-items:flex-end;gap:16px;margin-bottom:12px}
.hero-pct{font-size:48px;font-weight:700;line-height:1;font-variant-numeric:tabular-nums}
.muted{color:#5b6777}
.bar{height:10px;background:#d7dee6;border-radius:999px;overflow:hidden;margin-bottom:24px}
.bar>span{display:block;height:100%;background:#1f8a4c}
.toc{display:flex;flex-wrap:wrap;gap:8px 14px;margin:0 0 32px;padding:12px 14px;background:#fff;border:1px solid #d7dee6;border-radius:10px}
.toc a{color:#164e8a;text-decoration:none}
.toc a:hover{text-decoration:underline}
section{margin:36px 0;padding:20px;background:#fff;border:1px solid #d7dee6;border-radius:12px}
.h-pct{font-size:16px;font-weight:650;margin-left:8px}
table{width:100%;border-collapse:collapse;margin:12px 0 0}
th,td{padding:7px 8px;text-align:left;border-bottom:1px solid #e6ebf0;vertical-align:middle}
th{font-size:12px;text-transform:uppercase;letter-spacing:.03em;color:#5b6777;font-weight:600}
.num{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
table.files{margin-top:20px}
a{color:#164e8a;text-decoration:none}
a:hover{text-decoration:underline}
.good{color:#1f8a4c}
.mid{color:#9a6b00}
.bad{color:#b42318}
.minibar{display:inline-block;width:72px;height:7px;background:#d7dee6;border-radius:999px;overflow:hidden;vertical-align:middle}
.minibar>span{display:block;height:100%}
.minibar>.good{background:#1f8a4c}
.minibar>.mid{background:#d4a017}
.minibar>.bad{background:#d64545}
.legend{display:flex;gap:12px;font-size:13px;color:#5b6777}
.legend span{padding:2px 8px;border-radius:4px}
.legend .cov,.src tr.cov td.code{background:#e7f6ec}
.legend .uncov,.src tr.uncov td.code{background:#fdecea}
.legend .partial,.src tr.partial td.code{background:#fff4d6}
.src{margin-top:16px;overflow:auto;border:1px solid #d7dee6;border-radius:8px;background:#fff}
.src table{margin:0;font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
.src td{border:0;padding:0 12px 0 0;vertical-align:top;white-space:pre}
.src .ln{width:1%;padding:0 12px;text-align:right;color:#8b97a6;user-select:none;background:#f7f8fa;border-right:1px solid #e6ebf0}
.src tr.cov .ln{background:#d9f0e1}
.src tr.uncov .ln{background:#f8d7d4}
.src tr.partial .ln{background:#ffe9b3}
`

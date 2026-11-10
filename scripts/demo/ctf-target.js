// 一个刻意"可被 HTTP 发现"的迷你 CTF 靶场（只用于本地验证钦原的闭环）。
//
// 设计：只用 http_request 就能走通，考察的是"发现 → 追查 → 拿证据"这条链：
//   GET /                     → 首页，暗示有个 robots.txt
//   GET /robots.txt           → Disallow: /backup/
//   GET /backup/              → 403，但响应头 X-Backup-Dir: /backup/2024/
//   GET /backup/2024/         → 目录列表，含 /backup/2024/flag.txt.bak
//   GET /backup/2024/flag.txt.bak → 真正的 flag（QY{...}）
// 干扰项：/admin 返回 401（没有可绕过的路径），避免"一猜就中"。
const http = require('http');

const FLAG = 'QY{l0c4l_ctf_for_qinyuan_2026}';
const routes = {
  '/': [200, 'text/html', '<html><head><title>Acme Notes</title></head><body><h1>Acme Notes</h1><p>服务上线公告。</p><!-- ops note: 别忘了 robots.txt --></body></html>'],
  '/robots.txt': [200, 'text/plain', 'User-agent: *\nDisallow: /backup/\nDisallow: /admin\n'],
  '/backup/': [403, 'text/plain', 'forbidden'],
  '/backup/2024/': [200, 'text/html', '<html><body><h2>Index of /backup/2024/</h2><ul><li><a href="flag.txt.bak">flag.txt.bak</a></li><li><a href="notes.md">notes.md</a></li></ul></body></html>'],
  '/backup/2024/notes.md': [200, 'text/markdown', '# 运维笔记\n\n- 备份目录忘了加鉴权（待修）\n- flag.txt.bak 是迁移前的旧备份\n'],
  '/backup/2024/flag.txt.bak': [200, 'text/plain', FLAG + '\n'],
  '/admin': [401, 'text/plain', 'unauthorized'],
  '/admin/login': [200, 'text/html', '<html><body><form method="post" action="/admin/login"><input name="user"><input name="pass" type="password"></form></body></html>'],
};

const srv = http.createServer((req, res) => {
  const url = new URL(req.url, 'http://127.0.0.1');
  const path = url.pathname;
  // 目录列表靠 X-Backup-Dir 指路：模拟"目录可枚举但直连 403"的常见配置。
  if (path === '/backup/' || path === '/backup') {
    res.writeHead(403, { 'Content-Type': 'text/plain', 'X-Backup-Dir': '/backup/2024/' });
    res.end('forbidden');
    return;
  }
  const r = routes[path];
  if (!r) {
    res.writeHead(404, { 'Content-Type': 'text/plain' });
    res.end('not found');
    return;
  }
  res.writeHead(r[0], { 'Content-Type': r[1], 'Server': 'nginx/1.24.0', 'X-Powered-By': 'acme-notes' });
  res.end(r[2]);
});

srv.listen(8080, '127.0.0.1', () => {
  console.log('CTF 靶场：http://127.0.0.1:8080/  (flag 在 /backup/2024/flag.txt.bak)');
});

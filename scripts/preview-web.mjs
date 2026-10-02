#!/usr/bin/env node

// 本地预览 Expo web 的静态导出产物（apps/app/dist-web）。
//
// 为什么不能直接用 `npx serve` / `python -m http.server`：
// Expo 静态导出把**动态路由**落成了带方括号的占位文件，例如
//   dist-web/product/[id].html
//   dist-web/order/[id].html
// 通用静态服务器只会把它当普通文件，请求 /product/<uuid> 会 404。
// 这个脚本做两件通用服务器不做的事：
//   1. 干净 URL：/products      → products.html
//   2. 动态路由：/product/<任意> → product/[id].html（把该段参数交给客户端路由去解析）
//
// 用法：
//   node scripts/preview-web.mjs [--port 8081] [--dir apps/app/dist-web]

import { readFile, stat } from 'node:fs/promises';
import { createServer } from 'node:http';
import { extname, join, normalize, resolve, sep } from 'node:path';

const args = process.argv.slice(2);
function argValue(name, fallback) {
  const i = args.indexOf(name);
  return i >= 0 && args[i + 1] ? args[i + 1] : fallback;
}

const PORT = Number(argValue('--port', '8081'));
const ROOT = resolve(argValue('--dir', 'apps/app/dist-web'));

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.map': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.webp': 'image/webp',
  '.gif': 'image/gif',
  '.ico': 'image/x-icon',
  '.woff': 'font/woff',
  '.woff2': 'font/woff2',
  '.ttf': 'font/ttf',
  '.otf': 'font/otf',
  '.txt': 'text/plain; charset=utf-8',
};

async function isFile(p) {
  try {
    return (await stat(p)).isFile();
  } catch {
    return false;
  }
}

/**
 * 把一个 URL 路径解析到磁盘文件。返回 null 表示没找到。
 *
 * 解析顺序（第一个命中的胜出）：
 *   1. 直接命中文件（/assets/xxx.js）
 *   2. 补 .html（/products → products.html）
 *   3. 补 /index.html（/ 或某个目录）
 *   4. 逐段替换成 [..] 占位（/product/<uuid> → product/[id].html）
 */
async function resolvePath(urlPath) {
  // 先做一次规范化并确保没有越出 ROOT —— 路径穿越必须挡掉。
  const safe = normalize(decodeURIComponent(urlPath)).replace(/^([/\\])+/, '');
  const base = resolve(ROOT, safe);
  if (base !== ROOT && !base.startsWith(ROOT + sep)) return null;

  const candidates = [];
  if (!safe) {
    candidates.push(join(ROOT, 'index.html'));
  } else {
    candidates.push(base);
    if (!extname(base)) {
      candidates.push(`${base}.html`);
      candidates.push(join(base, 'index.html'));
    }
  }
  for (const c of candidates) {
    if (await isFile(c)) return c;
  }

  // 动态路由：把最后一段（以及更靠前的段）换成方括号占位。
  // Expo 用文件名里的 [id] 作占位符，静态导出后仍是这个字面名。
  const segments = safe.split(/[/\\]+/).filter(Boolean);
  for (let i = 0; i < segments.length; i++) {
    const swapped = segments.slice();
    swapped[i] = '[id]';
    const p = `${join(ROOT, ...swapped)}.html`;
    if (await isFile(p)) return p;
  }
  // 兜底：整段都不匹配就交给 404 页面。
  const notFound = join(ROOT, '+not-found.html');
  return (await isFile(notFound)) ? notFound : null;
}

const server = createServer(async (req, res) => {
  const urlPath = (req.url || '/').split('?')[0];
  let file = null;
  try {
    file = await resolvePath(urlPath);
  } catch {
    file = null;
  }

  if (!file) {
    res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end('404');
    return;
  }

  try {
    const body = await readFile(file);
    const isNotFoundPage = file.endsWith('+not-found.html');
    res.writeHead(isNotFoundPage ? 404 : 200, {
      'Content-Type': MIME[extname(file).toLowerCase()] ?? 'application/octet-stream',
      'Cache-Control': 'no-store',
    });
    res.end(body);
  } catch (err) {
    res.writeHead(500, { 'Content-Type': 'text/plain; charset=utf-8' });
    res.end(`500 ${err.message}`);
  }
});

server.listen(PORT, '127.0.0.1', () => {
  // 输出到 stdout，调用方把它重定向到日志文件（本机 /dev/null 不可靠）。
  console.log(`preview-web listening on http://localhost:${PORT}  root=${ROOT}`);
});

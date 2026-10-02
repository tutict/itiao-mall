#!/usr/bin/env node
/**
 * 从 OpenAPI 契约生成 TS 类型。
 *
 * 为什么自己写这一层、而不是直接跑 `openapi-typescript` 的 CLI：
 *
 *   TypeScript 7.0 换成 Go 原生编译器后**不再提供 JS 编译器 API**（`ts.factory.*`，
 *   官方排在 7.1 才补）。openapi-typescript 内部 `require('typescript')` 拿到的却是
 *   工作区根的 TS 7.0.2，`ts.factory` 是 undefined，导入即崩：
 *     TypeError: Cannot read properties of undefined (reading 'createKeywordTypeNode')
 *
 *   它的 `bin/cli.js` 是 ESM（走 dist/index.mjs），而 ESM 的解析钩子必须另起
 *   loader 进程，复杂且脆。好在它同时提供 `dist/index.cjs`，于是这里改为 CJS 直调 API，
 *   并在 require 之前把 `typescript` 的解析劫持到根依赖里的 npm 别名 `typescript5`
 *   （= typescript@5.9.3，纯工具侧，只为喂它 JS 编译器 API）。
 *   应用自身的 typescript@7.0.2 不受任何影响。
 *
 * 为什么劫持是"全局"而不是只挑 openapi-typescript 的调用：本进程的唯一职责就是生成
 * 类型文件，里面不该有任何东西需要 TS 7。做成全局，行为可预测、不依赖路径匹配。
 *
 * 用法:
 *   node tools/codegen/gen-ts-types.cjs <spec.yaml> <out.d.ts>
 */
'use strict';

const Module = require('node:module');
const fs = require('node:fs');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

const [specArg, outArg] = process.argv.slice(2);
if (!specArg || !outArg) {
  console.error('用法: node tools/codegen/gen-ts-types.cjs <spec.yaml> <out.d.ts>');
  process.exit(2);
}

// ---- 1. 把 `typescript` 指向工具侧的 TS 5.9.3 ----
let ts5Entry;
try {
  ts5Entry = require.resolve('typescript5');
} catch {
  console.error(
    '缺少 typescript5。它应当是 package.json 里的 npm 别名：\n' +
      '  "typescript5": "npm:typescript@5.9.3"\n' +
      '先跑 pnpm install。',
  );
  process.exit(1);
}

const originalResolve = Module._resolveFilename;
Module._resolveFilename = function resolveFilename(request, parent, isMain, options) {
  if (request === 'typescript') return ts5Entry;
  return originalResolve.call(this, request, parent, isMain, options);
};

// 佐证劫持确实生效 —— 万一以后 pnpm 或 TS 的行为变了，这里会立刻暴露，而不是
// 又变回一个看不懂的 "Cannot read properties of undefined"。
const ts = require('typescript');
if (typeof ts.factory?.createKeywordTypeNode !== 'function') {
  console.error(`劫持后的 typescript 仍不具备编译器 API（version=${ts.version}）。`);
  process.exit(1);
}

// ---- 2. 直调 API（与 CLI 的 generateSchema 保持同样的输出拼装）----
const mod = require('openapi-typescript');
const openapiTS = mod.default || mod;
const { astToString, COMMENT_HEADER } = mod;

// 注意：必须传 URL 对象，不能传裸路径字符串。
// openapi-typescript 的 CLI 也是这么做的（`new URL(input, CWD)`）——
// 传字符串会走到别的分支，报 "Unsupported schema format, expected `openapi: 3.x`"。
const spec = pathToFileURL(path.resolve(specArg));
const out = path.resolve(outArg);

(async () => {
  const ast = await openapiTS(spec);
  const text = `${COMMENT_HEADER}${astToString(ast)}`;
  fs.mkdirSync(path.dirname(out), { recursive: true });
  fs.writeFileSync(out, text, 'utf8');
  console.log(`   -> ${path.relative(process.cwd(), out)}  (${text.split('\n').length} 行)`);
})().catch((err) => {
  console.error(`生成失败: ${err && err.message ? err.message : err}`);
  process.exit(1);
});

// 构建脚本：web/src/js/main.js → <out>/assets/app.js（+ sourcemap）
//
// 设计约束（docs/TD.md §9「工程约束」）：
//   - 产物必须**可读**：不压缩，保留 sourcemap，出问题能直接对着源码看。
//   - 产物必须**确定**：同样的输入得到逐字节相同的输出，`make web-check`
//     才能用「重建后逐字节比对」的方式发现「改了源码忘了重建」。
//     因此这里不写时间戳、不写绝对路径、不引入随机量。
//   - 产物目录可参数化（--out），web-check 用同层深的临时目录重建后 diff，
//     保证 esbuild 算出的 sourcemap 相对路径与真实产物一致。
import { build } from 'esbuild';
import { cp, mkdir, rm } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const srcDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(srcDir, '..', '..');

function argValue(flag, fallback) {
  const i = process.argv.indexOf(flag);
  return i >= 0 && process.argv[i + 1] ? path.resolve(process.argv[i + 1]) : fallback;
}

const outDir = argValue('--out', path.resolve(srcDir, '..', 'dist'));

async function main() {
  await rm(outDir, { recursive: true, force: true });
  await mkdir(path.join(outDir, 'assets'), { recursive: true });

  await build({
    absWorkingDir: srcDir,
    entryPoints: [path.join(srcDir, 'js', 'main.js')],
    outfile: path.join(outDir, 'assets', 'app.js'),
    bundle: true,
    format: 'esm',
    target: ['es2020'],
    // 不压缩：产物要能直接在浏览器里读（TD §9 工程约束）。
    minify: false,
    // sourcemap 内容嵌在 .map 里（sourcesContent），只交 dist 也能对着源码看。
    sourcemap: true,
    legalComments: 'inline',
    charset: 'utf8',
    logLevel: 'warning',
  });

  // 静态页面与样式是手写的，原样随产物一起提交。
  await cp(path.join(srcDir, 'index.html'), path.join(outDir, 'index.html'));
  await cp(path.join(srcDir, 'styles.css'), path.join(outDir, 'assets', 'styles.css'));

  const rel = path.relative(repoRoot, outDir) || outDir;
  process.stdout.write(`web-build: ${rel} (index.html, assets/app.js, assets/app.js.map, assets/styles.css)\n`);
}

main().catch((err) => {
  process.stderr.write(`web-build 失败: ${err && err.stack ? err.stack : err}\n`);
  process.exit(1);
});

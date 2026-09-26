#!/usr/bin/env node
// Сборка live-адаптера в esm-бандл для node-проверок (ws-smoke, adapter.test).
// Упаковщик берётся из node_modules: vite 8 таскает rolldown; esbuild — если
// он всё-таки есть в дереве зависимостей. Отдельных пакетов не заводим.
import { spawnSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const entry = join(here, 'adapter-entry.ts')
const out = `--outfile=${join(here, 'adapter.bundle.mjs')}`
const bins = {
  esbuild: join(here, '..', 'node_modules', '.bin', 'esbuild'),
  rolldown: join(here, '..', 'node_modules', '.bin', 'rolldown'),
}
if (existsSync(bins.esbuild)) {
  const res = spawnSync(bins.esbuild,
    [entry, '--bundle', '--format=esm', '--platform=node', out], { stdio: 'inherit' })
  process.exit(res.status ?? 1)
}
if (existsSync(bins.rolldown)) {
  // rolldown CLI: input positional, --file выход, --format esm
  const res = spawnSync(bins.rolldown, [entry, '--file', join(here, 'adapter.bundle.mjs'),
    '--format', 'esm'], { stdio: 'inherit' })
  process.exit(res.status ?? 1)
}
console.error('нет упаковщика (esbuild/rolldown) в node_modules — выполните npm ci в dashboard/')
process.exit(1)

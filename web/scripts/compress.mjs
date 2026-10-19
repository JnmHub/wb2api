// Pre-compress the production bundle (gzip + brotli) so the Go server can hand
// out the compressed bytes directly instead of compressing per request.
import { readdirSync, statSync, readFileSync, writeFileSync } from 'node:fs'
import { join, extname } from 'node:path'
import { gzipSync, brotliCompressSync, constants } from 'node:zlib'

const dist = new URL('../../internal/webui/dist/', import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')
const compressible = new Set(['.js', '.css', '.html', '.svg', '.json', '.txt', '.map'])
let total = 0, gz = 0, br = 0
function walk(dir) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) { walk(p); continue }
    if (!compressible.has(extname(name))) continue
    const raw = readFileSync(p)
    if (raw.length < 512) continue
    const g = gzipSync(raw, { level: 9 })
    const b = brotliCompressSync(raw, { params: { [constants.BROTLI_PARAM_QUALITY]: 11 } })
    writeFileSync(p + '.gz', g)
    writeFileSync(p + '.br', b)
    total += raw.length; gz += g.length; br += b.length
  }
}
walk(dist)
writeFileSync(join(dist, '.keep'), '')
console.log(`compressed: ${(total / 1024).toFixed(0)} KB raw -> ${(gz / 1024).toFixed(0)} KB gzip, ${(br / 1024).toFixed(0)} KB brotli`)

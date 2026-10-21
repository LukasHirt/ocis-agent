import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'

const dist = new URL('../dist', import.meta.url).pathname
const manifestPath = join(dist, 'manifest.json')

if (!existsSync(manifestPath)) {
  console.error(`check-dist: ${manifestPath} is missing`)
  process.exit(1)
}

const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'))
if (typeof manifest.entrypoint !== 'string' || manifest.entrypoint === '') {
  console.error('check-dist: manifest.json has no entrypoint')
  process.exit(1)
}

const entry = join(dist, manifest.entrypoint)
if (!existsSync(entry)) {
  console.error(`check-dist: entrypoint ${manifest.entrypoint} does not exist in dist/`)
  process.exit(1)
}

const jsFiles = (dir) =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? jsFiles(join(dir, e.name)) : /\.m?js$/.test(e.name) ? [join(dir, e.name)] : []
  )
const jsDir = join(dist, 'js')
if (!existsSync(jsDir) || !jsFiles(jsDir).some((f) => /\.agent-main\s*\{/.test(readFileSync(f, 'utf8')))) {
  console.error('check-dist: shared styles are missing from the bundle')
  process.exit(1)
}

console.log(`check-dist: ok (${manifest.entrypoint})`)

import type { RouteLocationRaw } from 'vue-router'
import { APPID } from '../../appid'

// fileRoute is the route of a memory file: its name split at '/', for the
// repeatable :name+ parameter.
export const fileRoute = (name: string): RouteLocationRaw => ({
  name: `${APPID}-memory-file`,
  params: { name: name.split('/') }
})

export const listRoute = (): RouteLocationRaw => ({ name: `${APPID}-memory` })

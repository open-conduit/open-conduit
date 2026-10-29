import { defineConfig, type Plugin } from 'vite'
import react from '@vitejs/plugin-react'
import http from 'node:http'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

// Read-only bridge to a local Open Conduit daemon. The management API listens
// on a unix socket and needs the operator token, which must never reach the
// browser, so the dev server attaches it here. Only GET is forwarded: the
// prototype cannot mutate real Work or configuration through this path.
function liveDaemon(): Plugin {
  const state = process.env.OPEN_CONDUIT_STATE_DIR ?? path.join(os.homedir(), '.local/state/open-conduit')
  const socketPath = path.join(state, 'management.sock')
  const tokenPath = path.join(state, 'private', 'operator.token')
  return {
    name: 'open-conduit-live-readonly',
    configureServer(server) {
      server.middlewares.use('/live/v1/', (req, res) => {
        if (req.method !== 'GET') {
          res.statusCode = 405
          res.end(JSON.stringify({ error: { code: 'read_only', message: 'the prototype bridge forwards GET only' } }))
          return
        }
        let token: string
        try {
          // The token file is {"token_id", "token"}; the bearer is the token.
          token = JSON.parse(fs.readFileSync(tokenPath, 'utf8')).token
        } catch {
          res.statusCode = 503
          res.end(JSON.stringify({ error: { code: 'no_daemon', message: 'operator token unavailable' } }))
          return
        }
        const upstream = http.request(
          { socketPath, path: '/v1/' + (req.url ?? '').replace(/^\//, ''), method: 'GET', headers: { Authorization: 'Bearer ' + token } },
          (up) => {
            res.statusCode = up.statusCode ?? 502
            res.setHeader('Content-Type', up.headers['content-type'] ?? 'application/json')
            up.pipe(res)
          },
        )
        upstream.on('error', () => {
          res.statusCode = 503
          res.end(JSON.stringify({ error: { code: 'no_daemon', message: 'daemon not reachable at ' + socketPath } }))
        })
        upstream.end()
      })
    },
  }
}

export default defineConfig({ plugins: [react(), liveDaemon()] })

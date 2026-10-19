import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api, onUnauthorized, setCsrf } from './api'

const json = (status: number, body: unknown) =>
  Promise.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }))

afterEach(() => {
  vi.unstubAllGlobals()
  setCsrf('')
})

describe('api()', () => {
  it('sends the CSRF header on writes only', async () => {
    const fetchMock = vi.fn(() => json(200, { ok: true }))
    vi.stubGlobal('fetch', fetchMock)
    setCsrf('tok123')
    await api('GET', '/api/v2/accounts')
    await api('PATCH', '/api/v2/accounts/x', { weight: 2 })
    const calls = fetchMock.mock.calls as unknown as [string, RequestInit][]
    expect((calls[0][1].headers as Record<string, string>)['X-WB-CSRF']).toBeUndefined()
    expect((calls[1][1].headers as Record<string, string>)['X-WB-CSRF']).toBe('tok123')
    expect((calls[1][1].headers as Record<string, string>)['Content-Type']).toBe('application/json')
    expect(calls[1][1].credentials).toBe('same-origin')
    expect(calls[1][1].body).toBe('{"weight":2}')
  })

  it('turns an error body into an ApiError', async () => {
    vi.stubGlobal('fetch', () => json(400, { error: { code: 'bad', message: 'weight must be between 1 and 100' } }))
    await expect(api('PATCH', '/x', {})).rejects.toMatchObject({ status: 400, code: 'bad', message: 'weight must be between 1 and 100' })
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('<html>oops</html>', { status: 502 })))
    await expect(api('GET', '/x')).rejects.toBeInstanceOf(ApiError)
  })

  it('signs out only when the session is really gone', async () => {
    const fn = vi.fn()
    const off = onUnauthorized(fn)
    vi.stubGlobal('fetch', () => json(401, { error: { code: 'login_required', message: 'panel login required' } }))
    await expect(api('GET', '/api/v2/accounts')).rejects.toThrow()
    expect(fn).toHaveBeenCalledTimes(1)

    vi.stubGlobal('fetch', () => json(401, { error: { code: 'invalid_password', message: 'current password is wrong' } }))
    await expect(api('POST', '/api/v2/session/password', {})).rejects.toThrow('current password is wrong')
    expect(fn).toHaveBeenCalledTimes(1) // a wrong password must not log the user out
    off()
  })
})

import { describe, expect, it, vi } from 'vitest'

import { api, APIError } from './api'

describe('api', () => {
  it('maps a non-object error response to a stable APIError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: false,
      status: 502,
      statusText: 'Bad Gateway',
      json: async () => null,
    } as Response))

    await expect(api('/api/fail')).rejects.toMatchObject({
      name: 'APIError',
      status: 502,
      code: 'request_failed',
      message: 'Bad Gateway',
    })
  })
})

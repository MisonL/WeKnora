import assert from 'node:assert/strict'
import test from 'node:test'

import { structuredServerError } from './requestError.ts'

// The app's 413 body names the real limit; it must reach the caller intact so
// the interceptor does not replace it with the proxy's file-size guess.
test('an app error envelope is recognised with its code and message', () => {
  assert.deepEqual(
    structuredServerError({
      success: false,
      error: { code: 1011, message: '请求体超过 33554432 字节上限，请减少内容后重试', details: 'payload too large' },
    }),
    { code: 1011, message: '请求体超过 33554432 字节上限，请减少内容后重试', details: 'payload too large' },
  )
})

// nginx answers 413 with HTML, and other 5xx bodies may be strings; none of
// those carry a code, so the caller keeps its own message.
test('a body without the envelope is not mistaken for a server error', () => {
  for (const body of [undefined, null, '', '<html>413 Request Entity Too Large</html>', { message: 'nope' },
    { error: 'text' }, { error: { message: 'no code' } }, { error: { code: '1011' } }]) {
    assert.equal(structuredServerError(body), undefined, JSON.stringify(body))
  }
})
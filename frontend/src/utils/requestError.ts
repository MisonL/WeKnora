// Reading the server's own error envelope out of a response body.
//
// Backend failures answer `{ success: false, error: { code, message, details } }`.
// A proxy refusal (nginx's own 413) does not: its body is HTML or plain text.
// Telling the two apart matters where the client would otherwise replace the
// server's message with a client-side guess.

export interface StructuredServerError {
  code: number
  message?: string
  details?: string
}

/**
 * The app's error envelope, when the body carries one. Returns undefined for
 * a proxy/HTML body so the caller keeps its own fallback text.
 */
export function structuredServerError(data: unknown): StructuredServerError | undefined {
  if (!data || typeof data !== 'object') return undefined
  const error = (data as { error?: unknown }).error
  if (!error || typeof error !== 'object') return undefined
  const { code, message, details } = error as { code?: unknown; message?: unknown; details?: unknown }
  if (typeof code !== 'number') return undefined
  return {
    code,
    ...(typeof message === 'string' ? { message } : {}),
    ...(typeof details === 'string' ? { details } : {}),
  }
}
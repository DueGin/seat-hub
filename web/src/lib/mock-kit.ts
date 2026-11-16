// MockKit hands feature mock modules (mock-tickets.ts, mock-reservations.ts)
// the shared helpers of the demo backend in mock.ts, so each feature keeps its
// demo routes in its own file. Handlers return undefined when a path is not
// theirs.
import type { User } from './types'

export type MockInput = Record<string, unknown>

export interface MockKit {
  /** JSON success envelope. */
  ok: (data?: unknown, status?: number) => Response
  /** Throws a JSON error response (never returns). */
  fail: (status: number, code: string, message: string) => never
  /** The signed-in demo user; throws 401 when signed out. */
  currentUser: () => User
  /** The signed-in demo user, or null when signed out (public routes). */
  optionalUser: () => User | null
  /** Every demo user (admin pages, lookups). */
  users: () => User[]
  /** ISO timestamp offset from the demo clock, in ms. */
  iso: (offset?: number) => string
}

export type MockHandler = (path: string, method: string, query: URLSearchParams, input: MockInput, kit: MockKit) => Response | undefined | Promise<Response | undefined>

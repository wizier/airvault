import { request } from './client';

/** Exchange the Web UI token for a session cookie. Skips the 401 redirect so a
 *  bad token surfaces inline on the login form instead of bouncing. */
export function login(token: string): Promise<void> {
  return request('/session', { method: 'POST', body: { token }, skipAuthRedirect: true });
}

/** Clear the session cookie. */
export function logout(): Promise<void> {
  return request('/session', { method: 'DELETE', skipAuthRedirect: true });
}

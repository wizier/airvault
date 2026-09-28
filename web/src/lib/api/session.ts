import { request } from './client';

// Skips the 401 redirect so a bad token surfaces inline on the login form.
export function login(token: string): Promise<void> {
  return request('/session', { method: 'POST', body: { token }, skipAuthRedirect: true });
}

export function logout(): Promise<void> {
  return request('/session', { method: 'DELETE', skipAuthRedirect: true });
}

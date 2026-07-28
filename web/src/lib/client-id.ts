let sequence = 0;

// Correlation ids must also work when AirVault is opened over plain HTTP on a
// LAN address. crypto.randomUUID() is unavailable there because it requires a
// secure browser context.
export function clientId(prefix: string): string {
  sequence += 1;
  const random = Math.random().toString(36).slice(2, 10);
  return `${prefix}-${Date.now().toString(36)}-${sequence.toString(36)}-${random}`;
}

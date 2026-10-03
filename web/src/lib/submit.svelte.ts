import { errorCode, isAbortError } from './api/client';
import { errorText, type ErrorTextKey } from './error-text';

/** A modal's request: busy while it runs, its failure kept as a stable code
 *  so the modal can react to specific verdicts (a wrong password, a locked
 *  phone) and not only show the text. */
export class Submit {
  busy = $state(false);
  code = $state<string | null>(null);

  #fallback: ErrorTextKey;
  #ctrl: AbortController | null = null;

  constructor(fallback: ErrorTextKey) {
    this.#fallback = fallback;
  }

  get failure(): string | null {
    return this.code && errorText(this.code, this.#fallback);
  }

  /** Resolves true once the action succeeded. */
  async run(action: (signal: AbortSignal) => Promise<unknown>): Promise<boolean> {
    const ctrl = new AbortController();
    this.#ctrl = ctrl;
    this.busy = true;
    this.code = null;
    try {
      await action(ctrl.signal);
      return true;
    } catch (err) {
      if (!isAbortError(err)) this.code = errorCode(err, this.#fallback);
      return false;
    } finally {
      if (this.#ctrl === ctrl) this.#ctrl = null;
      this.busy = false;
    }
  }

  /** For modals that drop the request when closed instead of waiting for it. */
  abort(): void {
    this.#ctrl?.abort();
  }
}

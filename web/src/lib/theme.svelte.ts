// Theme state as a rune module so any component can read/toggle it reactively.
// The initial class is applied pre-paint by the inline script in index.html;
// here we mirror that into reactive state and persist changes to localStorage.

type Theme = 'light' | 'dark';

const STORAGE_KEY = 'airvault-theme';

// daisyUI theme names for each mode (see src/app.css).
const DAISY_THEME: Record<Theme, string> = {
  light: 'airvault',
  dark: 'airvaultdark',
};

function currentFromDom(): Theme {
  return document.documentElement.getAttribute('data-theme') === 'airvault'
    ? 'light'
    : 'dark';
}

class ThemeStore {
  value = $state<Theme>(currentFromDom());

  get isDark(): boolean {
    return this.value === 'dark';
  }

  set(next: Theme): void {
    this.value = next;
    document.documentElement.setAttribute('data-theme', DAISY_THEME[next]);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      /* private mode / storage disabled — ignore */
    }
  }

  toggle(): void {
    this.set(this.value === 'dark' ? 'light' : 'dark');
  }
}

export const theme = new ThemeStore();

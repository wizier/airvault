import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';

const target = document.getElementById('app');
if (!target) {
  throw new Error('AirVault: #app mount target not found');
}

mount(App, { target });

(() => {
  const button = /** @type {HTMLButtonElement} */ (document.getElementById('copy-token'));
  const token = /** @type {HTMLElement} */ (document.getElementById('created-token'));
  if (!button || !token) return;
  button.addEventListener('click', async () => {
    const value = token.textContent || '';
    await window['copyTextWithStatus'](button, value);
  });
})();

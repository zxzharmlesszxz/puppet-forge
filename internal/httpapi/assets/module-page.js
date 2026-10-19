/** @type {(text: string) => Promise<boolean>} */
const copyText = window['copyTextToClipboard'];
const versionSelect = /** @type {HTMLSelectElement|null} */ (document.getElementById('version-select'));
if (versionSelect) {
  versionSelect.addEventListener('change', () => {
    const url = new URL(window.location.href);
    url.searchParams.set('version', versionSelect.value);
    window.location.href = url.toString();
  });
}
const copyVersionLink = document.getElementById('copy-version-link');
if (copyVersionLink && versionSelect) {
  copyVersionLink.addEventListener('click', async () => {
    const url = new URL(window.location.href);
    url.search = '';
    url.hash = '';
    url.searchParams.set('version', versionSelect.value);
    if (await copyText(url.toString())) {
      copyVersionLink.classList.add('is-copied');
      copyVersionLink.title = 'Copied';
      copyVersionLink.setAttribute('aria-label', 'Link copied');
      window.setTimeout(() => {
        copyVersionLink.classList.remove('is-copied');
        copyVersionLink.title = 'Copy link to this version';
        copyVersionLink.setAttribute('aria-label', 'Copy link to this version');
      }, 1400);
    } else {
      copyVersionLink.title = 'Copy failed';
      copyVersionLink.setAttribute('aria-label', 'Copy failed');
    }
  });
}
const installPanel = /** @type {HTMLDetailsElement|null} */ (document.getElementById('install'));
const openInstallPanel = () => {
  if (installPanel && window.location.hash === '#install') installPanel.open = true;
};
document.querySelector('a[href="#install"]')?.addEventListener('click', () => {
  if (installPanel) installPanel.open = true;
});
window.addEventListener('hashchange', openInstallPanel);
openInstallPanel();
document.querySelectorAll('button.copy-button').forEach((button) => {
  button.addEventListener('click', async () => {
    const code = button.closest('.code-window')?.querySelector('code');
    if (!code) return;
    const text = code.textContent || '';
    await window['copyTextWithStatus'](button, text);
  });
});
document.querySelectorAll('.readme img').forEach((element) => {
  const image = /** @type {HTMLImageElement} */ (element);
  const replaceBrokenImage = () => {
    if (image.naturalWidth !== 0) return;
    const alternative = image.getAttribute('alt')?.trim() || 'Image unavailable';
    image.replaceWith(document.createTextNode(alternative));
  };
  image.addEventListener('error', replaceBrokenImage, {once: true});
  if (image.complete) replaceBrokenImage();
});

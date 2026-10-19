window['copyTextToClipboard'] = async (text) => {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {}
  }

  const field = document.createElement('textarea');
  field.value = text;
  field.setAttribute('readonly', '');
  field.setAttribute('aria-hidden', 'true');
  field.style.position = 'fixed';
  field.style.left = '-9999px';
  field.style.opacity = '0';
  document.body.appendChild(field);
  field.focus();
  field.select();
  field.setSelectionRange(0, field.value.length);

  let copied = false;
  try {
    // noinspection JSDeprecatedSymbols -- required fallback for non-secure HTTP origins.
    copied = document.execCommand('copy');
  } catch {}
  field.remove();
  return copied;
};

(() => {
  const statuses = new WeakMap();
  window['copyTextWithStatus'] = async (button, text) => {
    const previous = statuses.get(button);
    if (previous) window.clearTimeout(previous.timer);
    const status = { original: previous?.original || button.textContent || 'Copy', timer: 0 };
    statuses.set(button, status);
    const copied = await window['copyTextToClipboard'](text);
    if (statuses.get(button) !== status) return copied;
    button.textContent = copied ? 'Copied' : 'Failed';
    status.timer = window.setTimeout(() => {
      button.textContent = status.original;
      statuses.delete(button);
    }, 1400);
    return copied;
  };
})();

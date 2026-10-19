(() => {
  const storagePrefix = "puppet-forge:page-size:";
  const allowedPageSizes = new Set(["10", "20", "50", "100"]);
  const isAllowedPageSize = (value) => typeof value === "string" && allowedPageSizes.has(value);

  const readLocalPageSize = (key) => {
    try {
      return window.localStorage.getItem(storagePrefix + key);
    } catch (_) {
      return null;
    }
  };
  const readCookie = (name) => {
    if (!name) return null;
    const prefix = `${encodeURIComponent(name)}=`;
    const entry = document.cookie.split("; ").find((value) => value.startsWith(prefix));
    try {
      return entry ? decodeURIComponent(entry.slice(prefix.length)) : null;
    } catch {
      return null;
    }
  };
  const persistPageSize = (form, value) => {
    const key = form.dataset.pageSizeKey;
    const name = form.dataset.pageSizeCookie;
    if (!key || !name || !isAllowedPageSize(value)) return;
    try {
      window.localStorage.setItem(storagePrefix + key, value);
    } catch (_) {}
    const secure = window.location.protocol === "https:" ? "; Secure" : "";
    document.cookie = `${encodeURIComponent(name)}=${encodeURIComponent(value)}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`;
  };
  const readStoredPageSize = (form) => {
    const cookieValue = readCookie(form.dataset.pageSizeCookie);
    const localValue = readLocalPageSize(form.dataset.pageSizeKey);
    return isAllowedPageSize(cookieValue) ? cookieValue : isAllowedPageSize(localValue) ? localValue : form.dataset.renderedPageSize;
  };

  window['pageSizePreferences'] = Object.freeze({
    isAllowed: isAllowedPageSize,
    read: readStoredPageSize,
    write: persistPageSize,
  });
})();

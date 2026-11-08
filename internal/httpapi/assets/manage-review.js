(() => {
  let request;
  let changeIndex = -1;
  let displayedURL = window.location.href;
  const positions = new Map();
  const current = () => document.getElementById("release-review");
  function filterFiles(value) {
    const root = current();
    if (!root) return;
    root.querySelectorAll("[data-review-file]").forEach((item) => {
      item.hidden = !String(item.getAttribute("data-review-file")).toLowerCase().includes(value.toLowerCase());
    });
    Array.from(root.querySelectorAll(".review-tree details")).reverse().forEach((directory) => {
      directory.parentElement.hidden = !Array.from(directory.querySelectorAll("[data-review-file]")).some((item) => !item.hidden);
      if (value) directory.open = true;
    });
  }
  document.addEventListener("input", (event) => {
    if (!(event.target instanceof HTMLInputElement)) return;
    if (event.target.matches("[data-review-filter]")) filterFiles(event.target.value);
  });
  document.addEventListener("change", (event) => {
    if (event.target instanceof HTMLInputElement && event.target.matches("[data-review-wrap]")) {
      current()?.classList.toggle("wrap-code", event.target.checked);
    }
  });
  async function navigate(url, push) {
    const root = current();
    if (!root) return;
    const filter = root.querySelector("[data-review-filter]");
    const filterValue = filter instanceof HTMLInputElement ? filter.value : "";
    const scroll = root.querySelector(".review-code-scroll");
    positions.set(displayedURL, scroll ? [scroll.scrollLeft, scroll.scrollTop] : [0, 0]);
    if (positions.size > 20) positions.delete(positions.keys().next().value);
    request?.abort();
    const controller = new AbortController();
    request = controller;
    root.setAttribute("aria-busy", "true");
    try {
      const response = await fetch(url, { signal: controller.signal, credentials: "same-origin" });
      if (!response.ok || response.redirected) {
        root.querySelector("#review-load-error").textContent = "Unable to open the file. Refresh the page to check access or release changes.";
        root.querySelector("#review-load-error").hidden = false;
        return;
      }
      const documentCopy = new DOMParser().parseFromString(await response.text(), "text/html");
      const replacement = documentCopy.getElementById("release-review");
      if (!replacement) return;
      if (controller.signal.aborted) return;
      replacement.classList.toggle("wrap-code", root.classList.contains("wrap-code"));
      const wrap = replacement.querySelector("[data-review-wrap]");
      if (wrap instanceof HTMLInputElement) wrap.checked = replacement.classList.contains("wrap-code");
      root.replaceWith(replacement);
      changeIndex = -1;
      const nextFilter = replacement.querySelector("[data-review-filter]");
      if (nextFilter instanceof HTMLInputElement) nextFilter.value = filterValue;
      filterFiles(filterValue);
      if (push) history.pushState(null, "", url);
      displayedURL = window.location.href;
      const position = positions.get(window.location.href);
      const nextScroll = replacement.querySelector(".review-code-scroll");
      if (position && nextScroll) { nextScroll.scrollLeft = position[0]; nextScroll.scrollTop = position[1]; }
      replacement.querySelector(".review-file")?.focus({ preventScroll: true });
    } catch {
      if (controller.signal.aborted) return;
      const notice = root.querySelector("#review-load-error");
      notice.textContent = "Unable to load the file. Try again.";
      notice.hidden = false;
    } finally {
      root.removeAttribute("aria-busy");
    }
  }
  document.addEventListener("click", (event) => {
    if (!(event.target instanceof Element) || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const step = event.target.closest("button[data-review-step]");
    if (step instanceof HTMLButtonElement) {
      const changes = current()?.querySelectorAll(".review-hunk");
      if (!changes?.length) return;
      changeIndex = (changeIndex + Number(step.dataset.reviewStep) + changes.length) % changes.length;
      const target = changes[changeIndex];
      if (target instanceof HTMLDetailsElement) { target.open = true; target.scrollIntoView({ block: "start" }); }
      return;
    }
    const link = event.target.closest("a[data-review-link]");
    if (!(link instanceof HTMLAnchorElement) || link.origin !== location.origin || link.pathname !== location.pathname) return;
    event.preventDefault();
    void navigate(link.href, true);
  });
  window.addEventListener("popstate", () => { void navigate(window.location.href, false); });
})();

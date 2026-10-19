document.addEventListener("DOMContentLoaded", () => {
  const notices = Array.from(document.querySelectorAll(".notice"));
  if (!notices.length) return;

  const currentURL = new URL(window.location.href);
  currentURL.searchParams.delete("message");
  currentURL.searchParams.delete("error");
  window.history.replaceState(null, "", currentURL.pathname + currentURL.search + currentURL.hash);

  notices.forEach((notice) => {
    const dismissButton = document.createElement("button");
    dismissButton.type = "button";
    dismissButton.className = "notice-dismiss";
    dismissButton.setAttribute("aria-label", "Dismiss notification");
    dismissButton.textContent = "×";
    notice.appendChild(dismissButton);

    let remaining = notice.classList.contains("error") ? 12000 : 6000;
    let startedAt = 0;
    let timer = 0;
    let dismissed = false;

    const pause = () => {
      if (!timer) return;
      window.clearTimeout(timer);
      timer = 0;
      remaining = Math.max(0, remaining - (Date.now() - startedAt));
    };
    const dismiss = () => {
      if (dismissed) return;
      dismissed = true;
      pause();
      notice.classList.add("is-dismissing");
      window.setTimeout(() => notice.remove(), 220);
    };
    const resume = () => {
      if (dismissed || timer || document.hidden || notice.matches(":hover") || notice.contains(document.activeElement)) return;
      if (remaining <= 0) {
        dismiss();
        return;
      }
      startedAt = Date.now();
      timer = window.setTimeout(dismiss, remaining);
    };

    dismissButton.addEventListener("click", dismiss);
    notice.addEventListener("mouseenter", pause);
    notice.addEventListener("mouseleave", resume);
    notice.addEventListener("focusin", pause);
    notice.addEventListener("focusout", () => window.setTimeout(resume));
    document.addEventListener("visibilitychange", () => document.hidden ? pause() : resume());
    resume();
  });
});

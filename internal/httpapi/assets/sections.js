(() => {
  const storageKey = document.body.dataset.sectionStorageKey;
  if (!storageKey) return;
  let openSections = new Set();
  try {
    openSections = new Set(JSON.parse(window.localStorage.getItem(storageKey) || "[]"));
  } catch {}
  const saveOpenSections = () => {
    try {
      window.localStorage.setItem(storageKey, JSON.stringify(Array.from(openSections)));
    } catch {}
  };
  const bindSections = (root) => {
    const sections = [];
    if (root instanceof HTMLDetailsElement && root.dataset.sectionKey) sections.push(root);
    root.querySelectorAll?.("details[data-section-key]").forEach((section) => sections.push(section));
    sections.forEach((section) => {
      if (section.dataset.sectionStateBound) return;
      section.dataset.sectionStateBound = "true";
      if (openSections.has(section.dataset.sectionKey)) section.open = true;
      section.addEventListener("toggle", () => {
        if (section.open) openSections.add(section.dataset.sectionKey);
        else openSections.delete(section.dataset.sectionKey);
        saveOpenSections();
      });
    });
  };
  bindSections(document);
  document.addEventListener("puppet-forge:list-updated", (event) => bindSections(event.target));
  document.addEventListener("submit", saveOpenSections);
  document.addEventListener("click", (event) => {
    if (event.target instanceof Element && event.target.closest(".module-actions")) event.stopPropagation();
  });
})();

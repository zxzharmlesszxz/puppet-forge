(() => {
  const openRow = (row) => {
    if (row?.dataset.href) window.location.assign(row.dataset.href);
  };
  document.addEventListener("click", (event) => {
    if (!(event.target instanceof Element) || event.target.closest("a, button, input, select, textarea, form")) return;
    openRow(event.target.closest("#publish-space-list tr[data-href]"));
  });
  document.addEventListener("keydown", (event) => {
    const row = event.target;
    if (!(row instanceof HTMLTableRowElement) || !row.matches("#publish-space-list tr[data-href]") || (event.key !== "Enter" && event.key !== " ")) return;
    event.preventDefault();
    openRow(row);
  });
})();

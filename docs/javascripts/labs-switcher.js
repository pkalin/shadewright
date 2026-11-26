document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  const switcher = document.querySelector("details.labs-switcher[open]");
  if (!switcher) return;
  switcher.open = false;
  switcher.querySelector("summary")?.focus();
});

document.addEventListener("click", (event) => {
  for (const switcher of document.querySelectorAll("details.labs-switcher[open]")) {
    if (!switcher.contains(event.target) || event.target.closest?.("a")) {
      switcher.open = false;
    }
  }
});

document.addEventListener("focusout", (event) => {
  const switcher = event.target.closest?.("details.labs-switcher[open]");
  if (switcher && event.relatedTarget && !switcher.contains(event.relatedTarget)) {
    switcher.open = false;
  }
});

const root = document.documentElement;
const languageButtons = document.querySelectorAll("[data-language]");
const menuButton = document.querySelector(".menu-button");
const navigation = document.querySelector(".site-nav");

function setLanguage(language) {
  const next = language === "zh" ? "zh" : "en";
  root.dataset.lang = next;
  root.lang = next === "zh" ? "zh-Hant" : "en";
  document.title = next === "zh"
    ? "AI-Intune — AI 機器的集中管理平臺"
    : "AI-Intune — Control every AI machine from one place";
  languageButtons.forEach((button) => {
    button.setAttribute("aria-pressed", String(button.dataset.language === next));
  });
  localStorage.setItem("ai-intune-language", next);
}

const savedLanguage = localStorage.getItem("ai-intune-language");
const requestedLanguage = new URLSearchParams(window.location.search).get("lang");
const browserLanguage = navigator.language.toLowerCase().startsWith("zh") ? "zh" : "en";
setLanguage(requestedLanguage || savedLanguage || browserLanguage);

languageButtons.forEach((button) => {
  button.addEventListener("click", () => setLanguage(button.dataset.language));
});

menuButton?.addEventListener("click", () => {
  const open = menuButton.getAttribute("aria-expanded") !== "true";
  menuButton.setAttribute("aria-expanded", String(open));
  navigation.classList.toggle("is-open", open);
});

document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && navigation?.classList.contains("is-open")) {
    menuButton.setAttribute("aria-expanded", "false");
    navigation.classList.remove("is-open");
    menuButton.focus();
  }
});

navigation?.querySelectorAll("a").forEach((link) => {
  link.addEventListener("click", () => {
    menuButton?.setAttribute("aria-expanded", "false");
    navigation.classList.remove("is-open");
  });
});

document.querySelectorAll("[data-year]").forEach((element) => {
  element.textContent = String(new Date().getFullYear());
});

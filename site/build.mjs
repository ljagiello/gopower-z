// Builds the powerz.jagiello.dev docs site.
//
// docs/*.html stays the single source of truth (it is also what GitHub Pages
// serves). This script keeps each page's own <style> and <main> content, drops
// the per-page link bar, rewrites internal links to clean URLs, and wraps the
// result in a shared shell: top bar, page sidebar, "on this page" rail, search
// and prev/next links. Pagefind then indexes dist/ (see package.json).

import { readFile, writeFile, mkdir, rm, cp } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as cheerio from "cheerio";

const here = dirname(fileURLToPath(import.meta.url));
const docsDir = join(here, "..", "docs");
const srcDir = join(here, "src");
const outDir = join(here, "dist");

const SITE = "https://powerz.jagiello.dev";
const REPO = "https://github.com/ljagiello/gopower-z";
// Google Analytics 4: account "PowerZ", property "powerz.jagiello.dev",
// web stream "powerz docs".
const GA_ID = "G-8F8SF7335Y";

// Sidebar order is also prev/next order.
const GROUPS = [
  {
    label: "Using powerz",
    pages: [
      { slug: "", label: "Overview", home: true },
      { slug: "cli", label: "CLI reference", file: "cli.html" },
    ],
  },
  {
    label: "How the meter works",
    pages: [
      { slug: "protocol", label: "USB protocol", file: "protocol.html" },
      { slug: "hardware", label: "Hardware", file: "hardware.html" },
      { slug: "official-software-re", label: "Official software", file: "official-software-re.html" },
      { slug: "firmware", label: "Firmware", file: "firmware.html" },
    ],
  },
];
const PAGES = GROUPS.flatMap((g) => g.pages);
const DOC_FILES = new Set(PAGES.filter((p) => p.file).map((p) => p.file.replace(/\.html$/, "")));

const esc = (s) =>
  String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const href = (slug) => "/" + slug;

// cli.html#x -> /cli#x, index.html -> /. External and in-page links are left alone.
function cleanHref(value) {
  const m = /^(?:\.\/)?([a-z0-9-]+)\.html(#.*)?$/i.exec(value);
  if (!m) return value;
  if (m[1] === "index") return "/" + (m[2] || "");
  if (DOC_FILES.has(m[1])) return "/" + m[1] + (m[2] || "");
  return value;
}

function slugify(text, used) {
  let base = text.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "") || "section";
  let id = base;
  for (let i = 2; used.has(id); i++) id = `${base}-${i}`;
  used.add(id);
  return id;
}

const icons = {
  search: '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="8.5" cy="8.5" r="5.5"/><path d="m13 13 4.5 4.5"/></svg>',
  github:
    '<svg viewBox="0 0 16 16" aria-hidden="true"><path fill="currentColor" stroke="none" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8Z"/></svg>',
  theme:
    '<svg viewBox="0 0 20 20" aria-hidden="true"><circle cx="10" cy="10" r="7"/><path d="M10 3a7 7 0 0 1 0 14Z" fill="currentColor"/></svg>',
  menu: '<svg viewBox="0 0 20 20" aria-hidden="true"><path d="M3 6h14M3 10h14M3 14h14"/></svg>',
};

// The mark: a USB-C receptacle outline with a VBUS rail through it.
const MARK =
  '<svg class="mark" viewBox="0 0 32 20" aria-hidden="true"><rect x="1.5" y="3" width="29" height="14" rx="7"/><path class="vbus" d="M7 10h4l2-4 3 8 2-4h7"/></svg>';

// The Google tag loads only on the production host, so local builds and
// Vercel preview deployments send nothing. site.js sends the custom events
// through window.gtag when it exists.
function analytics() {
  const host = new URL(SITE).hostname;
  return `<script>
(function () {
  if (location.hostname !== ${JSON.stringify(host)}) return;
  window.dataLayer = window.dataLayer || [];
  window.gtag = function () { dataLayer.push(arguments); };
  gtag("js", new Date());
  gtag("config", ${JSON.stringify(GA_ID)});
  var s = document.createElement("script");
  s.async = true;
  s.src = "https://www.googletagmanager.com/gtag/js?id=${GA_ID}";
  document.head.appendChild(s);
})();
</script>`;
}

function head({ title, description, path, extraStyle = "" }) {
  const url = SITE + path;
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(title)}</title>
<meta name="description" content="${esc(description)}">
<link rel="canonical" href="${url}">
<meta property="og:type" content="website">
<meta property="og:site_name" content="powerz docs">
<meta property="og:title" content="${esc(title)}">
<meta property="og:description" content="${esc(description)}">
<meta property="og:url" content="${url}">
<meta name="theme-color" content="#f4f6f7" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#0b1419" media="(prefers-color-scheme: dark)">
<link rel="icon" href="/favicon.svg" type="image/svg+xml">
<link rel="preload" href="/fonts/instrument-sans-latin-standard-normal.woff2" as="font" type="font/woff2" crossorigin>
<script>try{var t=localStorage.getItem("powerz-theme");if(t==="light"||t==="dark")document.documentElement.dataset.theme=t}catch(e){}</script>
${analytics()}
${extraStyle}
<link rel="stylesheet" href="/site.css">
</head>`;
}

function topbar() {
  return `<a class="skip" href="#content">Skip to content</a>
<header class="topbar">
  <button class="icon-btn menu-btn" type="button" aria-controls="sidebar" aria-expanded="false" aria-label="Open page list">${icons.menu}</button>
  <a class="brand" href="/">${MARK}<span>powerz</span></a>
  <button class="search-btn" type="button" data-open-search>${icons.search}<span class="label">Search the docs</span><kbd>/</kbd></button>
  <nav class="top-links" aria-label="Project">
    <a class="icon-btn" href="${REPO}" aria-label="Source on GitHub">${icons.github}</a>
    <button class="icon-btn theme-btn" type="button" aria-label="Switch colour theme">${icons.theme}</button>
  </nav>
</header>`;
}

function sidebar(current) {
  const groups = GROUPS.map(
    (g) => `<div class="side-group">
    <p class="side-label">${esc(g.label)}</p>
    <ul>${g.pages
      .map(
        (p) =>
          `<li><a href="${href(p.slug)}"${p.slug === current ? ' aria-current="page"' : ""}>${esc(p.label)}</a></li>`,
      )
      .join("")}</ul>
  </div>`,
  ).join("\n  ");
  return `<nav class="sidebar" id="sidebar" aria-label="Documentation">
  ${groups}
  <div class="side-group side-ext">
    <p class="side-label">Elsewhere</p>
    <ul>
      <li><a href="${REPO}/releases">Releases</a></li>
      <li><a href="https://pkg.go.dev/github.com/ljagiello/gopower-z">Go package docs</a></li>
      <li><a href="${REPO}">Source on GitHub</a></li>
    </ul>
  </div>
</nav>
<div class="scrim" hidden></div>`;
}

function searchDialog() {
  return `<dialog class="search" aria-label="Search the docs">
  <div class="search-head">
    <div id="search-ui"></div>
    <button class="search-close" type="button" aria-label="Close search">Esc</button>
  </div>
</dialog>`;
}

function footer() {
  return `<footer class="site-foot">
  <p>Apache License 2.0. Not affiliated with or endorsed by ChargerLAB.</p>
  <p><a href="${REPO}">github.com/ljagiello/gopower-z</a></p>
</footer>`;
}

function pager(slug) {
  const i = PAGES.findIndex((p) => p.slug === slug);
  const prev = PAGES[i - 1];
  const next = PAGES[i + 1];
  const link = (p, rel, word) =>
    p
      ? `<a class="pager-${rel}" href="${href(p.slug)}" rel="${rel}"><span>${word}</span>${esc(p.label)}</a>`
      : "<span></span>";
  return `<nav class="pager" aria-label="Previous and next page">${link(prev, "prev", "Previous")}${link(next, "next", "Next")}</nav>`;
}

async function buildDoc(page) {
  const raw = await readFile(join(docsDir, page.file), "utf8");
  const $ = cheerio.load(raw, { decodeEntities: false });

  const description = $('meta[name="description"]').attr("content") || "";
  const pageStyle = $("head style")
    .map((_, el) => `<style>${$(el).html()}</style>`)
    .get()
    .join("\n");
  const main = $("main").first();

  // The shell's sidebar replaces the per-page link bar.
  main.find(".docs-nav").remove();

  // The first <nav> in a page is its table of contents. Tag it so the shell
  // can hide it once the right-hand rail is on screen.
  main.find("nav").first().addClass("page-toc");

  main.find("a[href]").each((_, a) => {
    const el = $(a);
    el.attr("href", cleanHref(el.attr("href")));
  });

  // Shell comments in code blocks render muted so the commands stand out.
  // Only plain-text blocks that are not JSON output are touched.
  main.find("pre > code").each((_, el) => {
    const code = $(el);
    if (code.children().length || /^\s*[{[]/.test(code.text())) return;
    const marked = code
      .html()
      .split("\n")
      .map((line) => line.replace(/(^|\s)(#(?![0-9a-fx]+;).*)$/i, '$1<span class="c">$2</span>'))
      .join("\n");
    code.html(marked);
  });

  const used = new Set();
  main.find("[id]").each((_, el) => used.add($(el).attr("id")));
  main.find("h2, h3").each((_, el) => {
    const h = $(el);
    if (!h.attr("id")) h.attr("id", slugify(h.text(), used));
  });

  const h1 = main.find("h1").first().text().trim();
  const rail = main
    .find("h2")
    .map((_, el) => {
      const h = $(el);
      return `<li><a href="#${esc(h.attr("id"))}">${esc(h.text().trim())}</a></li>`;
    })
    .get()
    .join("");

  const title = `${h1 || page.label} | powerz`;
  const html = `${head({ title, description, path: href(page.slug), extraStyle: pageStyle })}
<body class="site site-doc">
${topbar()}
<div class="layout">
${sidebar(page.slug)}
<main class="doc" id="content" data-pagefind-body>
${main.html()}
${pager(page.slug)}
<p class="edit"><a href="${REPO}/blob/main/docs/${page.file}">View this page's source on GitHub</a></p>
</main>
<aside class="rail" aria-label="On this page">
  <p class="rail-label">On this page</p>
  <ul>${rail}</ul>
</aside>
</div>
${footer()}
${searchDialog()}
<script src="/site.js" defer></script>
</body>
</html>
`;
  await writeFile(join(outDir, `${page.slug}.html`), html);
}

async function buildHome() {
  const body = await readFile(join(srcDir, "home.html"), "utf8");
  const html = `${head({
    title: "powerz: a read-only CLI for the POWER-Z KM003C",
    description:
      "powerz is a read-only command-line client and Go library for the ChargerLAB POWER-Z KM003C USB-C power meter, built for people and AI agents: JSON output, structured errors, stable exit codes and a self-describing schema.",
    path: "/",
  })}
<body class="site site-home">
${topbar()}
<div class="layout layout-home">
${sidebar("")}
<main class="home" id="content" data-pagefind-body>
${body}
${pager("")}
</main>
</div>
${footer()}
${searchDialog()}
<script src="/site.js" defer></script>
</body>
</html>
`;
  await writeFile(join(outDir, "index.html"), html);
}

async function build404() {
  const html = `${head({ title: "Page not found | powerz", description: "This page does not exist.", path: "/404" })}
<body class="site site-home" data-page="404">
${topbar()}
<div class="layout layout-home">
${sidebar(null)}
<main class="home notfound" id="content">
  <h1>No page at this address</h1>
  <p>The link may point to an old <code>.html</code> name or a section that moved. Search the docs, or start from one of the pages in the list.</p>
  <p><a class="btn" href="/">Go to the overview</a></p>
</main>
</div>
${footer()}
${searchDialog()}
<script src="/site.js" defer></script>
</body>
</html>
`;
  await writeFile(join(outDir, "404.html"), html);
}

async function copyAssets() {
  await cp(join(srcDir, "site.css"), join(outDir, "site.css"));
  await cp(join(srcDir, "site.js"), join(outDir, "site.js"));
  await cp(join(srcDir, "favicon.svg"), join(outDir, "favicon.svg"));
  await mkdir(join(outDir, "fonts"), { recursive: true });
  const fonts = [
    ["@fontsource-variable/instrument-sans", "instrument-sans-latin-standard-normal.woff2"],
    ["@fontsource-variable/instrument-sans", "instrument-sans-latin-ext-standard-normal.woff2"],
    ["@fontsource-variable/instrument-sans", "instrument-sans-latin-standard-italic.woff2"],
    ["@fontsource/fragment-mono", "fragment-mono-latin-400-normal.woff2"],
    ["@fontsource/fragment-mono", "fragment-mono-latin-ext-400-normal.woff2"],
  ];
  for (const [pkg, file] of fonts) {
    await cp(join(here, "node_modules", pkg, "files", file), join(outDir, "fonts", file));
  }
}

await rm(outDir, { recursive: true, force: true });
await mkdir(outDir, { recursive: true });
await copyAssets();
await buildHome();
for (const page of PAGES.filter((p) => p.file)) await buildDoc(page);
await build404();
console.log(`built ${PAGES.length} pages into ${outDir}`);

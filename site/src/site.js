// powerz docs shell behaviour: theme, page list drawer, search, copy buttons,
// "on this page" highlighting, install tabs, the meter readout and analytics
// events.
(() => {
  const root = document.documentElement;
  const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;

  // ---- analytics ----
  // window.gtag exists only on the production host (see analytics() in
  // build.mjs). Page views, scrolls, outbound clicks and file downloads come
  // from GA4 enhanced measurement; the events below cover what it cannot see.
  // Custom parameters: theme, section, cta (registered as custom dimensions).
  const track = (name, params = {}) => {
    if (typeof window.gtag === "function") window.gtag("event", name, params);
  };
  if (document.body.dataset.page === "404") track("page_not_found");
  document.addEventListener("click", (e) => {
    const a = e.target.closest("a[href]");
    if (!a) return;
    if (a.dataset.cta) track("cta_click", { cta: a.dataset.cta });
    else if (a.closest(".rail")) track("select_content", { content_type: "toc", content_id: a.hash });
    else if (a.closest(".pager")) track("select_content", { content_type: "pager", content_id: a.getAttribute("href") });
    else if (a.closest("#search-ui")) {
      const url = new URL(a.href, location.href);
      track("select_content", { content_type: "search_result", content_id: url.pathname + url.hash });
    }
  });

  // ---- theme ----
  const themeBtn = document.querySelector(".theme-btn");
  const systemDark = matchMedia("(prefers-color-scheme: dark)");
  const currentTheme = () => root.dataset.theme || (systemDark.matches ? "dark" : "light");
  const labelTheme = () => {
    if (!themeBtn) return;
    const next = currentTheme() === "dark" ? "light" : "dark";
    themeBtn.setAttribute("aria-label", `Switch to ${next} theme`);
    themeBtn.title = `Switch to ${next} theme`;
  };
  themeBtn?.addEventListener("click", () => {
    const next = currentTheme() === "dark" ? "light" : "dark";
    root.dataset.theme = next;
    try { localStorage.setItem("powerz-theme", next); } catch (e) {}
    labelTheme();
    track("theme_change", { theme: next });
  });
  systemDark.addEventListener?.("change", labelTheme);
  labelTheme();

  // ---- page list drawer (narrow screens) ----
  const menuBtn = document.querySelector(".menu-btn");
  const scrim = document.querySelector(".scrim");
  const setNav = (open) => {
    document.body.classList.toggle("nav-open", open);
    menuBtn?.setAttribute("aria-expanded", String(open));
    menuBtn?.setAttribute("aria-label", open ? "Close page list" : "Open page list");
    if (scrim) scrim.hidden = !open;
    if (open) document.querySelector(".sidebar a")?.focus();
  };
  menuBtn?.addEventListener("click", () => setNav(!document.body.classList.contains("nav-open")));
  scrim?.addEventListener("click", () => setNav(false));
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && document.body.classList.contains("nav-open")) {
      setNav(false);
      menuBtn?.focus();
    }
  });

  // ---- search (Pagefind UI, loaded on first open) ----
  const dialog = document.querySelector("dialog.search");
  let searchReady = null;
  const loadSearch = () => {
    if (searchReady) return searchReady;
    searchReady = new Promise((resolve, reject) => {
      const css = document.createElement("link");
      css.rel = "stylesheet";
      css.href = "/pagefind/pagefind-ui.css";
      document.head.appendChild(css);
      const s = document.createElement("script");
      s.src = "/pagefind/pagefind-ui.js";
      s.onload = () => {
        // eslint-disable-next-line no-undef
        new PagefindUI({
          element: "#search-ui",
          showSubResults: true,
          showImages: false,
          resetStyles: false,
          translations: { placeholder: "Search the docs" },
          // The index stores file names (cli.html); the site serves clean URLs.
          processResult: (r) => {
            const clean = (u) => u.replace(/\.html(?=$|#)/, "");
            r.url = clean(r.url);
            (r.sub_results || []).forEach((s) => (s.url = clean(s.url)));
            return r;
          },
        });
        resolve();
      };
      s.onerror = () => {
        document.getElementById("search-ui").innerHTML =
          '<p style="margin:8px 4px;color:var(--muted)">Search is not available in this build. Run <code>npm run build</code> in <code>site/</code> to create the index.</p>';
        reject(new Error("pagefind not found"));
      };
      document.head.appendChild(s);
    });
    return searchReady;
  };
  const openSearch = async () => {
    if (!dialog) return;
    dialog.showModal();
    try {
      await loadSearch();
    } catch (e) {
      return;
    }
    requestAnimationFrame(() => dialog.querySelector("input")?.focus());
  };
  document.querySelectorAll("[data-open-search]").forEach((b) => b.addEventListener("click", openSearch));
  // One "search" event per settled query, not per keystroke.
  let searchTimer = 0;
  let lastTerm = "";
  dialog?.addEventListener("input", (e) => {
    if (!e.target.matches("input")) return;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      const term = e.target.value.trim().slice(0, 100);
      if (term.length < 2 || term === lastTerm) return;
      lastTerm = term;
      track("search", { search_term: term });
    }, 1000);
  });
  dialog?.querySelector(".search-close")?.addEventListener("click", () => dialog.close());
  dialog?.addEventListener("click", (e) => { if (e.target === dialog) dialog.close(); });
  document.addEventListener("keydown", (e) => {
    const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName) || e.target.isContentEditable;
    if ((e.key === "/" && !typing) || (e.key.toLowerCase() === "k" && (e.metaKey || e.ctrlKey))) {
      if (dialog?.open) return;
      e.preventDefault();
      openSearch();
    }
  });
  // Leaving the page from a result should not leave the dialog open on back-navigation.
  window.addEventListener("pageshow", () => dialog?.open && dialog.close());

  // ---- copy buttons ----
  const sectionHeads = [...document.querySelectorAll("main :is(h2, h3)[id]")];
  const sectionOf = (el) => {
    let id = "top";
    for (const h of sectionHeads) {
      if (h.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING) id = h.id;
      else break;
    }
    return id;
  };
  document.querySelectorAll("main pre").forEach((pre) => {
    const wrap = document.createElement("div");
    wrap.className = "code-wrap";
    pre.parentNode.insertBefore(wrap, pre);
    wrap.appendChild(pre);
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "copy";
    btn.textContent = "Copy";
    btn.setAttribute("aria-label", "Copy code");
    btn.addEventListener("click", async () => {
      try {
        await navigator.clipboard.writeText(pre.innerText.replace(/\n$/, ""));
        btn.textContent = "Copied";
        btn.classList.add("done");
        track("copy_code", { section: sectionOf(pre) });
      } catch (e) {
        btn.textContent = "Select and copy";
      }
      setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("done"); }, 1600);
    });
    wrap.appendChild(btn);
  });

  // ---- "on this page" highlighting ----
  const railLinks = [...document.querySelectorAll(".rail a")];
  if (railLinks.length) {
    const byId = new Map(railLinks.map((a) => [decodeURIComponent(a.hash.slice(1)), a]));
    const heads = [...byId.keys()].map((id) => document.getElementById(id)).filter(Boolean);
    const rail = document.querySelector(".rail");
    let queued = false;
    const pick = () => {
      queued = false;
      if (!rail.offsetParent) return; // rail hidden on narrow screens
      // A section counts as current once its heading passes 30% of the viewport.
      const line = Math.max(140, innerHeight * 0.3);
      let current = null;
      for (const h of heads) {
        if (h.getBoundingClientRect().top < line) current = h;
        else break;
      }
      railLinks.forEach((a) => a.classList.remove("active"));
      const link = current && byId.get(current.id);
      if (link) {
        link.classList.add("active");
        const r = link.getBoundingClientRect();
        const rr = rail.getBoundingClientRect();
        if (r.top < rr.top + 40 || r.bottom > rr.bottom - 40) {
          rail.scrollTop += r.top - rr.top - rr.height / 3;
        }
      }
    };
    const schedule = () => {
      if (!queued) { queued = true; requestAnimationFrame(pick); }
    };
    addEventListener("scroll", schedule, { passive: true });
    addEventListener("resize", schedule);
    pick();
  }

  // ---- install tabs ----
  document.querySelectorAll("[data-tabs]").forEach((box) => {
    const tabs = [...box.querySelectorAll('[role="tab"]')];
    const select = (tab) => {
      tabs.forEach((t) => {
        const on = t === tab;
        t.setAttribute("aria-selected", String(on));
        t.tabIndex = on ? 0 : -1;
        document.getElementById(t.getAttribute("aria-controls")).hidden = !on;
      });
    };
    const choose = (tab) => {
      select(tab);
      track("select_content", { content_type: "install_tab", content_id: tab.id.replace(/^tab-/, "") });
    };
    tabs.forEach((t, i) => {
      t.addEventListener("click", () => choose(t));
      t.addEventListener("keydown", (e) => {
        const d = e.key === "ArrowRight" ? 1 : e.key === "ArrowLeft" ? -1 : 0;
        if (!d) return;
        const next = tabs[(i + d + tabs.length) % tabs.length];
        choose(next);
        next.focus();
      });
    });
    if (/Linux/.test(navigator.userAgent) && !/Android/.test(navigator.userAgent)) select(tabs[1]);
  });

  // ---- meter readout: digits settle once on load, like the meter after plug-in ----
  const digits = [...document.querySelectorAll(".digits[data-value]")];
  if (digits.length && !reduceMotion) {
    const start = performance.now();
    const dur = 900;
    const frame = (now) => {
      const t = Math.min(1, (now - start) / dur);
      const spread = Math.pow(1 - t, 2.2);
      digits.forEach((el) => {
        const v = parseFloat(el.dataset.value);
        const places = +el.dataset.places;
        const jitter = (Math.random() - 0.5) * 2 * spread * Math.max(v, 0.2) * 0.6;
        el.textContent = Math.max(0, v * (1 - spread) + jitter).toFixed(places);
      });
      if (t < 1) requestAnimationFrame(frame);
      else digits.forEach((el) => (el.textContent = (+el.dataset.value).toFixed(+el.dataset.places)));
    };
    requestAnimationFrame(frame);
  }
})();

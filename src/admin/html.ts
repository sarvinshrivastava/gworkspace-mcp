/**
 * The only way the dashboard produces HTML. Every interpolated value is
 * escaped unless it is already Safe, because most of what the dashboard shows
 * is attacker-reachable: Drive folder names (anyone can share a folder named
 * `<img onerror=…>` with this account) and audit resource IDs taken from tool
 * arguments a prompt-injected model chose.
 */

export type Safe = { readonly __html: string };

const ENTITIES: Record<string, string> = {
  "&": "&amp;",
  "<": "&lt;",
  ">": "&gt;",
  '"': "&quot;",
  "'": "&#39;",
};

export function esc(value: unknown): string {
  return String(value).replace(/[&<>"']/g, (c) => ENTITIES[c]!);
}

const isSafe = (value: unknown): value is Safe =>
  typeof value === "object" && value !== null && "__html" in value;

function render(value: unknown): string {
  if (value === null || value === undefined || value === false) return "";
  if (Array.isArray(value)) return value.map(render).join("");
  if (isSafe(value)) return value.__html;
  return esc(value);
}

export function html(strings: TemplateStringsArray, ...values: unknown[]): Safe {
  let out = strings[0]!;
  values.forEach((value, i) => {
    out += render(value) + strings[i + 1]!;
  });
  return { __html: out };
}

export function raw(s: string): Safe {
  return { __html: s };
}

const STYLE = `
:root{color-scheme:light dark;--on-brand:#fff;--ink:#1e2124;--muted:#5b636b;--line:#d9dee3;--bg:#f6f8fa;--card:#fff;--brand:#1a5893;--warn:#8f5e12;--warn-bg:#faf1e3;--bad:#cc3129;--bad-bg:#faeaea;--ok:#1f7a4d}
@media (prefers-color-scheme:dark){:root{--on-brand:#0e1824;--ink:#e8edf2;--muted:#a9b3bd;--line:#2f3740;--bg:#0e1824;--card:#152536;--brand:#6ea8e0;--warn:#e0a04a;--warn-bg:#3a2c15;--bad:#f0685f;--bad-bg:#3a1b1a;--ok:#5fc08e}}
*{box-sizing:border-box}body{margin:0;font:15px/1.5 system-ui,-apple-system,Segoe UI,sans-serif;color:var(--ink);background:var(--bg)}
header{display:flex;flex-wrap:wrap;gap:8px 16px;align-items:center;padding:12px 16px;border-bottom:1px solid var(--line);background:var(--card)}
header strong{margin-right:auto}a{color:var(--brand)}header a{text-decoration:none}
main{max-width:1040px;margin:0 auto;padding:16px}
section{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin:0 0 16px}
h1{font-size:20px;margin:0 0 12px}h2{font-size:16px;margin:0 0 10px}
table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);vertical-align:top}
th{font-weight:600;color:var(--muted);font-size:13px}
code{font-family:ui-monospace,Menlo,monospace;font-size:.87em;overflow-wrap:anywhere}
input[type=text],input[type=password],select{font:inherit;padding:6px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--ink);max-width:100%}
button{font:inherit;padding:6px 12px;border-radius:6px;border:1px solid var(--brand);background:var(--brand);color:var(--on-brand);cursor:pointer}
button:hover{filter:brightness(1.1)}button.quiet:hover,button.danger:hover{background:var(--bg)}
:focus-visible{outline:2px solid var(--brand);outline-offset:2px}input[type=checkbox]{accent-color:var(--brand)}
button.quiet{background:transparent;color:var(--brand)}button.danger{border-color:var(--bad);background:transparent;color:var(--bad)}
form.inline{display:inline}.row{display:flex;flex-wrap:wrap;gap:8px;align-items:center}.row.spaced{margin-top:12px}
.badge{display:inline-block;font-size:12px;padding:1px 8px;border-radius:999px;border:1px solid var(--line);color:var(--muted)}
.badge.bad{border-color:var(--bad);color:var(--bad)}.badge.warn{border-color:var(--warn);color:var(--warn)}.badge.ok{border-color:var(--ok);color:var(--ok)}
.note{padding:10px 12px;border-radius:8px;background:var(--warn-bg);border:1px solid var(--warn);margin:0 0 12px}
.error{padding:10px 12px;border-radius:8px;background:var(--bad-bg);border:1px solid var(--bad);margin:0 0 12px}
.secret{padding:12px;border-radius:8px;border:2px solid var(--ok);margin:0 0 12px}
.muted{color:var(--muted)}.wrap{overflow-x:auto}
`;

export function layout(title: string, body: Safe, opts: { nav?: boolean; csrf?: string } = {}): string {
  const nav = opts.nav
    ? html`<header><strong>gworkspace-mcp</strong><a href="/">Agents</a><a href="/audit">Audit</a>
        <form class="inline" method="post" action="/cache/flush"><input type="hidden" name="_csrf" value="${opts.csrf ?? ""}"><button class="quiet" type="submit">Flush cache</button></form>
        <form class="inline" method="post" action="/logout"><input type="hidden" name="_csrf" value="${opts.csrf ?? ""}"><button class="quiet" type="submit">Log out</button></form></header>`
    : html``;
  return `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${esc(title)} · gworkspace-mcp</title><style>${STYLE}</style></head><body>${nav.__html}<main>${body.__html}</main></body></html>`;
}

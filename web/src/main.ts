import m from "mithril"

type Problem = {
  title: string
  detail: string
  code: string
  request_id: string
  remediation?: {summary: string; steps?: string[]}
  violations?: Array<{pointer: string; message: string}>
}

type Folder = {id: string; name: string; parent_id?: string; path: string; summary?: string}
type Document = {
  id: string
  folder_id?: string
  title: string
  format: string
  content?: string
  revision: string
  status: string
  etag: string
  updated_at: string
}
type Job = {id: string; kind: string; resource_id: string; status: string; attempts: number; error?: Problem; updated_at: string}
type SearchHit = {kind: string; id: string; title: string; snippet?: string; score: number; source: string; revision?: string}
type Entity = {id: string; name: string; kind: string; updated_at: string}
type Relation = {id: string; from_entity_id: string; to_entity_id: string; predicate: string; status: string}
type Conflict = {id: string; fact_a_id: string; fact_b_id: string; status: string; resolution?: string}
type RenamePlan = {
  id: string
  status: string
  preview: {
    operations: Array<{resource_type: string; from: string; to: string}>
    replacements: Array<{document_id: string; revision: string; occurrences: number}>
    structured_references: number
    blockers?: Array<{resource: string; reason: string; location?: string}>
  }
}

const state = {
  csrf: sessionStorage.getItem("manifold-csrf") ?? "",
  authenticated: false,
  status: "checking",
  problem: null as Problem | null,
}

async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set("Accept", "application/json")
  if (init.body) headers.set("Content-Type", "application/json")
  if (state.csrf && init.method && !["GET", "HEAD"].includes(init.method)) {
    headers.set("X-CSRF-Token", state.csrf)
  }
  const response = await fetch(path, {...init, headers, credentials: "same-origin"})
  const payload = response.status === 204 ? null : await response.json().catch(() => null)
  if (!response.ok) {
    const error = (payload ?? {
      title: "Request failed",
      detail: `The server returned HTTP ${response.status}.`,
      code: "http_error",
      request_id: response.headers.get("X-Request-ID") ?? "unknown",
    }) as Problem
    state.problem = error
    m.redraw()
    throw error
  }
  state.problem = null
  return payload as T
}

function idempotencyKey(): string {
  const alphabet = "0123456789abcdefghijklmnopqrstuv"
  const bytes = crypto.getRandomValues(new Uint8Array(20))
  return Array.from(bytes, value => alphabet[value % alphabet.length]).join("")
}

const ProblemView: m.Component<{problem: Problem}> = {
  view: ({attrs}) => m(".problem", [
    m("h3", attrs.problem.title),
    m("p", attrs.problem.detail),
    attrs.problem.remediation && m("div", [
      m("strong", attrs.problem.remediation.summary),
      attrs.problem.remediation.steps?.length && m("ol", attrs.problem.remediation.steps.map(step => m("li", step))),
    ]),
    attrs.problem.violations?.map(item => m("p", [m("code", item.pointer), ` — ${item.message}`])),
    m("div.mono", `${attrs.problem.code} · request ${attrs.problem.request_id}`),
  ]),
}

const Login: m.Component = {
  view: () => {
    let keyInput: HTMLInputElement
    return m(".login-shell", m("main.login-card", [
      m(".wordmark", [m(".mark", {"aria-hidden": "true"}), m("span", "Manifold")]),
      m("h1", "Follow the evidence."),
      m("p.lede", "A private workbench for the documents, facts, and relationships your agents rely on."),
      state.problem && m(ProblemView, {problem: state.problem}),
      m("form", {
        onsubmit: async (event: SubmitEvent) => {
          event.preventDefault()
          const key = keyInput.value.trim()
          try {
            const result = await api<{csrf_token: string}>("/api/v1/ui/session", {
              method: "POST",
              body: JSON.stringify({api_key: key}),
            })
            keyInput.value = ""
            state.csrf = result.csrf_token
            sessionStorage.setItem("manifold-csrf", state.csrf)
            state.authenticated = true
            m.route.set("/explorer")
          } catch {
            // The semantic problem is rendered above.
          }
        },
      }, [
        m(".field", [
          m("label", {for: "api-key"}, "API key"),
          m("input", {
            id: "api-key",
            type: "password",
            autocomplete: "off",
            placeholder: "Paste a Manifold API key",
            oncreate: ({dom}) => { keyInput = dom as HTMLInputElement },
          }),
        ]),
        m("button.primary", {type: "submit"}, "Open workbench"),
      ]),
    ]))
  },
}

const navItems = [
  ["/explorer", "⌘", "Explorer"],
  ["/search", "⌕", "Search"],
  ["/jobs", "↻", "Jobs"],
  ["/graph", "⌁", "Graph"],
  ["/conflicts", "!", "Conflicts"],
  ["/renames", "↔", "Renames"],
  ["/system", "◫", "System"],
] as const

const Layout: m.Component = {
  view: ({children}) => {
    const current = m.route.get()
    return m(".app-shell", [
      m("aside.sidebar", [
        m(".wordmark", [m(".mark", {"aria-hidden": "true"}), m("span", "Manifold")]),
        m("nav.nav", {"aria-label": "Primary navigation"}, navItems.map(([href, icon, label]) =>
          m(m.route.Link, {href, class: current.startsWith(href) ? "active" : ""}, [
            m("span.mono", {"aria-hidden": "true"}, icon), " ", m("span", label),
          ]),
        )),
        m(".sidebar-foot", [
          m("div", "REST · OpenAPI 3.1"),
          m("a", {
            href: "https://github.com/iamwavecut/Manifold",
            target: "_blank",
            rel: "noreferrer",
          }, "AGPL source"),
        ]),
      ]),
      m(".workspace", [
        m("header.topbar", [
          m("h1", navItems.find(([href]) => current.startsWith(href))?.[2] ?? "Workbench"),
          m(`.status-dot.${state.status}`, state.status),
        ]),
        m("main.content", [
          state.problem && m(ProblemView, {problem: state.problem}),
          children,
        ]),
      ]),
    ])
  },
}

function pageHead(eyebrow: string, title: string, description: string): m.Children {
  return m(".page-head", m("div", [
    m(".eyebrow", eyebrow),
    m("h2", title),
    m("p", description),
  ]))
}

const Explorer: m.Component = {
  oninit: async vnode => {
    const component = vnode.state as ExplorerState
    await component.load()
  },
  view: vnode => {
    const component = vnode.state as ExplorerState
    return [
      pageHead("Canonical knowledge", "Explorer", "Read the source before following its derived facts."),
      m(".explorer", [
        m("section", [
          m(".eyebrow", "Folders"),
          m("button.tree-row", {class: component.folder === "" ? "active" : "", onclick: () => component.selectFolder("")}, "All documents"),
          component.folders.map(folder => m("button.tree-row", {
            class: component.folder === folder.id ? "active" : "",
            onclick: () => component.selectFolder(folder.id),
          }, [folder.name, m("small", folder.path)])),
          m(".eyebrow", {style: "margin-top:24px"}, "Documents"),
          component.documents.map(doc => m("button.list-row", {
            class: component.selected?.id === doc.id ? "active" : "",
            onclick: () => component.selectDocument(doc.id),
          }, [doc.title, m("small", `${doc.revision} · ${doc.status}`)])),
        ]),
        m("section.document-view", component.selected ? [
          m(".eyebrow", `${component.selected.id} · ${component.selected.revision}`),
          m("h2", component.selected.title),
          m(".document-content", component.selected.content || "No text content."),
        ] : m(".empty", "Select a document to read its canonical content.")),
        m("section.provenance", [
          m(".eyebrow", "Provenance ledger"),
          component.selected ? m(".evidence-ledger", [
            m(".evidence-item", [m("strong", "Current source"), m("p", `${component.selected.id}@${component.selected.revision}`)]),
            m(".evidence-item", [m("strong", "Processing state"), m("p", component.selected.status)]),
            m(".evidence-item", [m("strong", "Content identity"), m("p.mono", component.selected.etag)]),
          ]) : m(".empty", "Evidence appears with the selected document."),
        ]),
      ]),
    ]
  },
}

class ExplorerState {
  folders: Folder[] = []
  documents: Document[] = []
  selected: Document | null = null
  folder = ""

  async load() {
    const [folders, documents] = await Promise.all([
      api<{items: Folder[]}>("/api/v1/folders"),
      api<{items: Document[]}>("/api/v1/documents"),
    ])
    this.folders = folders.items ?? []
    this.documents = documents.items ?? []
    m.redraw()
  }
  async selectFolder(id: string) {
    this.folder = id
    const suffix = id ? `?folder_id=${encodeURIComponent(id)}` : ""
    this.documents = (await api<{items: Document[]}>(`/api/v1/documents${suffix}`)).items ?? []
    this.selected = null
    m.redraw()
  }
  async selectDocument(id: string) {
    this.selected = await api<Document>(`/api/v1/documents/${encodeURIComponent(id)}`)
    m.redraw()
  }
}

const SearchPage: m.Component = {
  view: vnode => {
    const component = vnode.state as SearchState
    return [
      pageHead("Evidence retrieval", "Search and context", "Fuse document recall with graph memory, then stop when the token budget is full."),
      m(".panel", [
        m(".toolbar", [
          m(".field.wide", [m("label", "Question"), m("input", {value: component.query, oninput: (e: InputEvent) => component.query = (e.target as HTMLInputElement).value, placeholder: "What changed in the authentication path?"})]),
          m(".field", [m("label", "Mode"), m("select", {value: component.mode, onchange: (e: Event) => component.mode = (e.target as HTMLSelectElement).value}, [
            "hybrid", "semantic", "lexical", "graph",
          ].map(value => m("option", {value}, value)))]),
          m("button.primary", {onclick: () => component.run(), disabled: component.loading}, component.loading ? "Tracing…" : "Trace evidence"),
        ]),
        component.results.length === 0 ? m(".empty", "Ask a question to trace evidence across documents and graph facts.") :
          component.results.map(hit => m(".result", [
            m(".result-thread"),
            m("div", [m("h3", hit.title), m("p", hit.snippet || hit.id), m("small.mono", `${hit.kind} · ${hit.source} · ${hit.revision ?? "current"}`)]),
            m(".score", hit.score.toFixed(4)),
          ])),
      ]),
    ]
  },
}

class SearchState {
  query = ""
  mode = "hybrid"
  loading = false
  results: SearchHit[] = []
  async run() {
    if (!this.query.trim()) return
    this.loading = true
    try {
      const result = await api<{items: SearchHit[]}>("/api/v1/search", {
        method: "POST",
        body: JSON.stringify({query: this.query, mode: this.mode, limit: 20}),
      })
      this.results = result.items ?? []
    } finally {
      this.loading = false
      m.redraw()
    }
  }
}

const JobsPage: m.Component = {
  oninit: vnode => (vnode.state as JobsState).load(),
  view: vnode => {
    const component = vnode.state as JobsState
    return [
      pageHead("Durable processing", "Jobs", "Every background transition keeps its failure reason and a concrete recovery path."),
      m(".panel", component.jobs.length ? m("table.table", [
        m("thead", m("tr", ["Job", "Operation", "Resource", "State", "Attempts", "Updated"].map(label => m("th", label)))),
        m("tbody", component.jobs.map(job => m("tr", [
          m("td.mono", job.id), m("td", job.kind), m("td", job.resource_id),
          m("td", m(`span.badge.${job.status}`, job.status)), m("td", String(job.attempts)),
          m("td", new Date(job.updated_at).toLocaleString()),
        ]))),
      ]) : m(".empty", "No background jobs yet.")),
    ]
  },
}
class JobsState {
  jobs: Job[] = []
  async load() {
    this.jobs = (await api<{items: Job[]}>("/api/v1/jobs?limit=100")).items ?? []
    m.redraw()
  }
}

const GraphPage: m.Component = {
  oninit: vnode => (vnode.state as GraphState).load(),
  view: vnode => {
    const component = vnode.state as GraphState
    return [
      pageHead("Derived memory", "Knowledge graph", "Entities are handles; facts and relations remain traceable to their sources."),
      m(".split", [
        m(".panel", [
          m("h3", "Entities"),
          component.entities.length ? m("table.table", [
            m("thead", m("tr", ["Slug", "Name", "Kind"].map(label => m("th", label)))),
            m("tbody", component.entities.map(entity => m("tr", [
              m("td.mono", entity.id), m("td", entity.name), m("td", m("span.badge", entity.kind)),
            ]))),
          ]) : m(".empty", "No entities have been recorded."),
        ]),
        m(".panel", [
          m("h3", "Relations"),
          component.relations.length ? m(".evidence-ledger", component.relations.map(relation =>
            m(".evidence-item", [m("strong", relation.predicate), m("p", `${relation.from_entity_id} → ${relation.to_entity_id}`)]),
          )) : m(".empty", "No explicit relations yet."),
        ]),
      ]),
    ]
  },
}
class GraphState {
  entities: Entity[] = []
  relations: Relation[] = []
  async load() {
    const [entities, relations] = await Promise.all([
      api<{items: Entity[]}>("/api/v1/entities?limit=100"),
      api<{items: Relation[]}>("/api/v1/relations?limit=100"),
    ])
    this.entities = entities.items ?? []
    this.relations = relations.items ?? []
    m.redraw()
  }
}

const ConflictsPage: m.Component = {
  oninit: vnode => (vnode.state as ConflictsState).load(),
  view: vnode => {
    const component = vnode.state as ConflictsState
    return [
      pageHead("Competing claims", "Conflicts", "Manifold keeps both claims visible until an authorized actor resolves them."),
      m(".panel", component.conflicts.length ? m("table.table", [
        m("thead", m("tr", ["Conflict", "Fact A", "Fact B", "State", "Resolution"].map(label => m("th", label)))),
        m("tbody", component.conflicts.map(conflict => m("tr", [
          m("td.mono", conflict.id), m("td.mono", conflict.fact_a_id), m("td.mono", conflict.fact_b_id),
          m("td", m(`span.badge.${conflict.status}`, conflict.status)), m("td", conflict.resolution || "—"),
        ]))),
      ]) : m(".empty", "No competing claims are awaiting review.")),
    ]
  },
}
class ConflictsState {
  conflicts: Conflict[] = []
  async load() {
    this.conflicts = (await api<{items: Conflict[]}>("/api/v1/conflicts?limit=100")).items ?? []
    m.redraw()
  }
}

const RenamesPage: m.Component = {
  view: vnode => {
    const component = vnode.state as RenameState
    return [
      pageHead("Identity maintenance", "Cascading rename", "Preview every structured and textual replacement before changing a public semantic ID."),
      m(".split", [
        m(".panel", [
          m(".field", [m("label", "Resource type"), m("select", {value: component.type, onchange: (e: Event) => component.type = (e.target as HTMLSelectElement).value}, [
            "folder", "document", "entity", "api_key",
          ].map(value => m("option", {value}, value)))]),
          m(".field", [m("label", "Current slug"), m("input", {value: component.from, oninput: (e: InputEvent) => component.from = (e.target as HTMLInputElement).value})]),
          m(".field", [m("label", "New slug"), m("input", {value: component.to, oninput: (e: InputEvent) => component.to = (e.target as HTMLInputElement).value})]),
          m("button.primary", {onclick: () => component.preview()}, "Build preview"),
        ]),
        m(".panel", component.plan ? [
          m(".eyebrow", `Plan ${component.plan.id}`),
          m("h3", `${component.plan.preview.structured_references} structured references`),
          m("p", `${component.plan.preview.replacements.reduce((sum, item) => sum + item.occurrences, 0)} textual replacements across ${component.plan.preview.replacements.length} documents.`),
          component.plan.preview.blockers?.length ? m(ProblemView, {problem: {
            title: "Unrewritable references",
            detail: component.plan.preview.blockers.map(item => item.resource).join(", "),
            code: "unrewritable_reference",
            request_id: component.plan.id,
          }}) : m("button.primary", {onclick: () => component.apply()}, "Apply reviewed plan"),
        ] : m(".empty", "A preview shows every active change before apply.")),
      ]),
    ]
  },
}
class RenameState {
  type = "document"
  from = ""
  to = ""
  plan: RenamePlan | null = null
  async preview() {
    this.plan = await api<RenamePlan>("/api/v1/rename-plans", {
      method: "POST",
      headers: {"Idempotency-Key": idempotencyKey()},
      body: JSON.stringify({operations: [{resource_type: this.type, from: this.from, to: this.to}]}),
    })
    m.redraw()
  }
  async apply() {
    if (!this.plan) return
    await api(`/api/v1/rename-plans/${this.plan.id}/apply`, {
      method: "POST",
      headers: {"Idempotency-Key": idempotencyKey()},
    })
    m.route.set("/jobs")
  }
}

const SystemPage: m.Component = {
  oninit: vnode => (vnode.state as SystemState).load(),
  view: vnode => {
    const component = vnode.state as SystemState
    return [
      pageHead("Runtime", "System", "Canonical storage remains available even when a derived layer is degraded."),
      component.data ? [
        m(".stat-grid", Object.entries(component.data.counts ?? {}).map(([key, value]) =>
          m(".stat", [m(".eyebrow", key.replaceAll("_", " ")), m("strong", String(value))]),
        )),
        m(".panel", [
          m("h3", "Components"),
          m("table.table", m("tbody", Object.entries(component.data.components ?? {}).map(([key, value]) =>
            m("tr", [m("td", key), m("td", m(`span.badge.${value}`, value))]),
          ))),
        ]),
      ] : m(".empty", "Loading component state…"),
    ]
  },
}
class SystemState {
  data: {state: string; counts: Record<string, number>; components: Record<string, string>} | null = null
  async load() {
    const data = await api<{state: string; counts: Record<string, number>; components: Record<string, string>}>("/api/v1/status")
    this.data = data
    state.status = data.state
    m.redraw()
  }
}

const Guard: m.RouteResolver = {
  async onmatch() {
    if (state.authenticated) return
    try {
      await api("/api/v1/ui/session")
      state.authenticated = true
    } catch {
      state.authenticated = false
      m.route.set("/login")
    }
  },
  render: vnode => m(Layout, vnode),
}

function guarded(component: m.Component): m.RouteResolver {
  return {
    onmatch: Guard.onmatch,
    render: () => m(Layout, m(component)),
  }
}

m.route.prefix = ""
m.route(document.getElementById("app")!, "/explorer", {
  "/login": Login,
  "/explorer": guarded(Explorer),
  "/search": guarded(SearchPage),
  "/jobs": guarded(JobsPage),
  "/graph": guarded(GraphPage),
  "/conflicts": guarded(ConflictsPage),
  "/renames": guarded(RenamesPage),
  "/system": guarded(SystemPage),
  "/:404...": {onmatch: () => m.route.set("/explorer")},
})

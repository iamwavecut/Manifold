import m from "mithril"

type Problem = {
  title: string
  detail: string
  code: string
  request_id: string
  remediation?: {summary: string; steps?: string[]}
  violations?: Array<{pointer: string; code: string; message: string; expected?: string; received?: unknown}>
  required_capabilities?: string[]
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
type SearchHit = {
  kind: string
  id: string
  canonical_ref?: string
  path?: string
  title: string
  snippet?: string
  score: number
  source: string
  revision?: string
}
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
    conflicts?: Array<{resource: string; reason: string; location?: string}>
    blockers?: Array<{resource: string; reason: string; location?: string}>
  }
}
type UISession = {
  csrf_token?: string
  capabilities: string[]
  expires_at?: string
  api_key_id?: string
}

const capabilities = {
  readDocuments: "read_documents",
  writeDocuments: "write_documents",
  search: "search",
  readGraph: "read_graph",
  writeGraph: "write_graph",
  manageConflicts: "manage_conflicts",
  admin: "admin",
} as const

type Capability = typeof capabilities[keyof typeof capabilities]

const state = {
  csrf: sessionStorage.getItem("manifold-csrf") ?? "",
  authenticated: false,
  capabilities: [] as string[],
  status: "checking",
  problem: null as Problem | null,
}

function isProblem(value: unknown): value is Problem {
  if (typeof value !== "object" || value === null) return false
  const candidate = value as Partial<Problem>
  return typeof candidate.code === "string" &&
    typeof candidate.title === "string" &&
    typeof candidate.detail === "string"
}

function clientProblem(
  code: string,
  title: string,
  detail: string,
  summary: string,
  steps: string[],
): Problem {
  return {
    title,
    detail,
    code,
    request_id: "not-issued",
    remediation: {summary, steps},
  }
}

function setProblem(problem: Problem): void {
  state.problem = problem
  m.redraw()
}

function runTask(task: () => Promise<void>, clearProblem = true): void {
  if (clearProblem) state.problem = null
  void task().catch(error => {
    if (!isProblem(error)) {
      state.problem = clientProblem(
        "client_error",
        "Workbench action failed",
        "The browser could not complete this action.",
        "Retry once; if the problem continues, report what you were doing.",
        ["Reload the workbench.", "Retry the action.", "Report the browser and action to the Manifold operator."],
      )
    }
    m.redraw()
  })
}

async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set("Accept", "application/json")
  if (init.body) headers.set("Content-Type", "application/json")
  if (state.csrf && init.method && !["GET", "HEAD"].includes(init.method)) {
    headers.set("X-CSRF-Token", state.csrf)
  }
  let response: Response
  try {
    response = await fetch(path, {...init, headers, credentials: "same-origin"})
  } catch {
    const error = clientProblem(
      "network_unavailable",
      "Manifold is unreachable",
      "The browser could not reach the Manifold API, so no server request ID was issued.",
      "Restore network access to this Manifold instance and retry.",
      ["Check that this page is online.", "Confirm the Manifold instance URL is reachable.", "Retry the action."],
    )
    setProblem(error)
    throw error
  }
  const payload = response.status === 204 ? null : await response.json().catch(() => null)
  if (!response.ok) {
    const error = (isProblem(payload) ? payload : {
      title: "Request failed",
      detail: `The server returned HTTP ${response.status}.`,
      code: "http_error",
      request_id: response.headers.get("X-Request-ID") ?? "unknown",
    }) as Problem
    state.problem = error
    m.redraw()
    if (response.status === 401 && path !== "/api/v1/ui/session") {
      clearSessionState()
      state.csrf = ""
      sessionStorage.removeItem("manifold-csrf")
      m.route.set("/login")
    }
    throw error
  }
  if (payload === null && response.status !== 204) {
    const error = clientProblem(
      "invalid_server_response",
      "Invalid server response",
      "Manifold returned a successful response without the JSON body required by this screen.",
      "Retry once, then report the response request ID to the Manifold operator.",
      ["Reload the workbench.", "Retry the action.", "Check the Manifold server logs if it happens again."],
    )
    error.request_id = response.headers.get("X-Request-ID") ?? "unknown"
    setProblem(error)
    throw error
  }
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
    attrs.problem.violations?.map(item => m("p", [
      m("code", item.pointer || "/"),
      ` — ${item.message}`,
      item.expected && m("small", ` Expected: ${item.expected}.`),
    ])),
    attrs.problem.required_capabilities?.length && m("p", [
      "Required capabilities: ",
      m("code", attrs.problem.required_capabilities.join(", ")),
    ]),
    m("div.mono", `${attrs.problem.code} · request ${attrs.problem.request_id}`),
  ]),
}

class Login implements m.ClassComponent {
  key = ""
  loading = false

  async submit() {
    if (this.loading) return
    this.loading = true
    m.redraw()
    try {
      const result = await api<UISession>("/api/v1/ui/session", {
        method: "POST",
        body: JSON.stringify({api_key: this.key.trim()}),
      })
      this.key = ""
      if (result.csrf_token) {
        state.csrf = result.csrf_token
        sessionStorage.setItem("manifold-csrf", state.csrf)
      }
      state.capabilities = result.capabilities ?? []
      state.authenticated = true
      state.status = "ready"
      m.route.set(firstAccessibleRoute())
    } finally {
      this.loading = false
      m.redraw()
    }
  }

  view() {
    return m(".login-shell", m("main.login-card", [
      m(".wordmark", [m(".mark", {"aria-hidden": "true"}), m("span", "Manifold")]),
      m("h1", "Follow the evidence."),
      m("p.lede", "A private workbench for the documents, facts, and relationships your agents rely on."),
      state.problem && m(ProblemView, {problem: state.problem}),
      m("form", {
        onsubmit: (event: SubmitEvent) => {
          event.preventDefault()
          runTask(() => this.submit())
        },
      }, [
        m(".field", [
          m("label", {for: "api-key"}, "API key"),
          m("input", {
            id: "api-key",
            type: "password",
            autocomplete: "off",
            placeholder: "Paste a Manifold API key",
            value: this.key,
            disabled: this.loading,
            oninput: (event: InputEvent) => {
              this.key = (event.target as HTMLInputElement).value
            },
          }),
        ]),
        m("button.primary", {
          type: "submit",
          disabled: this.loading || this.key.trim().length === 0,
        }, this.loading ? "Opening…" : "Open workbench"),
      ]),
    ]))
  }
}

const navItems = [
  {href: "/explorer", icon: "⌘", label: "Explorer", required: [capabilities.readDocuments]},
  {href: "/search", icon: "⌕", label: "Search", required: [capabilities.search]},
  {href: "/jobs", icon: "↻", label: "Jobs", required: [capabilities.readDocuments]},
  {href: "/graph", icon: "⌁", label: "Graph", required: [capabilities.readGraph]},
  {href: "/conflicts", icon: "!", label: "Conflicts", required: [capabilities.readGraph]},
  {
    href: "/renames",
    icon: "↔",
    label: "Renames",
    required: [capabilities.writeDocuments, capabilities.writeGraph],
  },
  {href: "/system", icon: "◫", label: "System", required: [capabilities.readDocuments]},
] as const satisfies ReadonlyArray<{
  href: string
  icon: string
  label: string
  required: readonly Capability[]
}>

function hasCapabilities(required: readonly Capability[]): boolean {
  if (state.capabilities.includes(capabilities.admin)) return true
  return required.every(capability => state.capabilities.includes(capability))
}

function firstAccessibleRoute(): string {
  return navItems.find(item => hasCapabilities(item.required))?.href ?? "/explorer"
}

function missingCapabilityProblem(required: readonly Capability[]): Problem {
  return {
    title: "Capability required",
    detail: "This API key does not grant every capability required by this workbench screen.",
    code: "missing_capability",
    request_id: "not-issued",
    required_capabilities: [...required],
    remediation: {
      summary: "Use a Manifold API key with the required capabilities.",
      steps: [
        "Open a screen allowed by the current key.",
        "Ask a Manifold administrator to issue an appropriately scoped key if this screen is required.",
      ],
    },
  }
}

const AccessDenied: m.Component = {
  view: () => pageHead(
    "Authorization",
    "Screen unavailable",
    "The current UI session is valid but does not authorize this screen.",
  ),
}

const Layout: m.Component = {
  view: ({children}) => {
    const current = m.route.get()
    const visibleNavItems = navItems.filter(item => hasCapabilities(item.required))
    return m(".app-shell", [
      m("aside.sidebar", [
        m(".wordmark", [m(".mark", {"aria-hidden": "true"}), m("span", "Manifold")]),
        m("nav.nav", {"aria-label": "Primary navigation"}, visibleNavItems.map(({href, icon, label}) =>
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
          m("h1", navItems.find(item => current.startsWith(item.href))?.label ?? "Workbench"),
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

const Explorer: m.FactoryComponent = () => {
  const component = new ExplorerState()
  return {
    oninit: () => runTask(() => component.load()),
    view: () => [
      pageHead("Canonical knowledge", "Explorer", "Read the source before following its derived facts."),
      component.loading ? m(".panel", m(".empty", "Loading canonical knowledge…")) : m(".explorer", [
        m("section", [
          m(".eyebrow", "Folders"),
          m("button.tree-row", {
            type: "button",
            class: component.folder === "" ? "active" : "",
            onclick: () => runTask(() => component.selectFolder("")),
          }, "All documents"),
          component.folders.map(folder => m("button.tree-row", {
            type: "button",
            class: component.folder === folder.id ? "active" : "",
            onclick: () => runTask(() => component.selectFolder(folder.id)),
          }, [folder.name, m("small", folder.path)])),
          m(".eyebrow", {style: "margin-top:24px"}, "Documents"),
          component.documents.map(doc => m("button.list-row", {
            type: "button",
            class: component.selected?.id === doc.id ? "active" : "",
            onclick: () => runTask(() => component.selectDocument(doc.id)),
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
    ],
  }
}

class ExplorerState {
  folders: Folder[] = []
  documents: Document[] = []
  selected: Document | null = null
  folder = ""
  loading = true
  private documentListRequest = 0
  private documentRequest = 0

  async load() {
    const request = ++this.documentListRequest
    try {
      const [folders, documents] = await Promise.all([
        api<{items: Folder[]}>("/api/v1/folders"),
        api<{items: Document[]}>("/api/v1/documents"),
      ])
      if (request !== this.documentListRequest) return
      this.folders = folders.items ?? []
      this.documents = documents.items ?? []
    } finally {
      this.loading = false
      m.redraw()
    }
  }

  async selectFolder(id: string) {
    this.folder = id
    const request = ++this.documentListRequest
    const suffix = id ? `?folder_id=${encodeURIComponent(id)}` : ""
    const documents = (await api<{items: Document[]}>(`/api/v1/documents${suffix}`)).items ?? []
    if (request !== this.documentListRequest) return
    this.documents = documents
    this.selected = null
    m.redraw()
  }

  async selectDocument(id: string) {
    const request = ++this.documentRequest
    const selected = await api<Document>(`/api/v1/documents/${encodeURIComponent(id)}`)
    if (request !== this.documentRequest) return
    this.selected = selected
    m.redraw()
  }
}

const SearchPage: m.FactoryComponent = () => {
  const component = new SearchState()
  return {
    oninit: () => { state.problem = null },
    view: () => [
      pageHead("Evidence retrieval", "Search and context", "Fuse document recall with graph memory, then stop when the token budget is full."),
      m("form.panel", {
        onsubmit: (event: SubmitEvent) => {
          event.preventDefault()
          runTask(() => component.run())
        },
      }, [
        m(".toolbar", [
          m(".field.wide", [
            m("label", {for: "search-query"}, "Question"),
            m("input", {
              id: "search-query",
              value: component.query,
              disabled: component.loading,
              oninput: (event: InputEvent) => {
                component.query = (event.target as HTMLInputElement).value
              },
              placeholder: "What changed in the authentication path?",
            }),
          ]),
          m(".field", [
            m("label", {for: "search-mode"}, "Mode"),
            m("select", {
              id: "search-mode",
              value: component.mode,
              disabled: component.loading,
              onchange: (event: Event) => {
                component.mode = (event.target as HTMLSelectElement).value
              },
            }, [
            "hybrid", "semantic", "lexical", "graph",
            ].map(value => m("option", {value}, value))),
          ]),
          m(".field", [
            m("label", {for: "search-scope"}, "Path scope"),
            m("input", {
              id: "search-scope",
              value: component.scopeGlob,
              disabled: component.loading,
              oninput: (event: InputEvent) => {
                component.scopeGlob = (event.target as HTMLInputElement).value
              },
              placeholder: "shared/**",
            }),
          ]),
          m("button.primary", {
            type: "submit",
            disabled: component.loading || component.query.trim().length === 0,
          }, component.loading ? "Tracing…" : "Trace evidence"),
        ]),
        component.degradedDependencies.length > 0 && m(".notice", [
          m("strong", "Partial results"),
          m("p", `Unavailable dependencies: ${component.degradedDependencies.join(", ")}.`),
        ]),
        component.results.length === 0 ? m(".empty", "Ask a question to trace evidence across documents and graph facts.") :
          component.results.map(hit => m(".result", [
            m(".result-thread"),
            m("div", [
              m("h3", hit.title),
              m("p", hit.snippet || hit.id),
              m("small.mono", `${hit.path ?? hit.id} · ${hit.source} · ${hit.revision ?? "current"}`),
              hit.canonical_ref && m("small.mono", hit.canonical_ref),
            ]),
            m(".score", hit.score.toFixed(4)),
          ])),
      ]),
    ],
  }
}

class SearchState {
  query = ""
  mode = "hybrid"
  scopeGlob = ""
  loading = false
  results: SearchHit[] = []
  degradedDependencies: string[] = []

  async run() {
    if (!this.query.trim() || this.loading) return
    this.loading = true
    m.redraw()
    try {
      const result = await api<{items: SearchHit[]; degraded_dependencies?: string[]}>("/api/v1/search", {
        method: "POST",
        body: JSON.stringify({
          query: this.query,
          mode: this.mode,
          scope_glob: this.scopeGlob,
          limit: 20,
        }),
      })
      this.results = result.items ?? []
      this.degradedDependencies = result.degraded_dependencies ?? []
    } finally {
      this.loading = false
      m.redraw()
    }
  }
}

const JobsPage: m.FactoryComponent = () => {
  const component = new JobsState()
  return {
    oninit: () => runTask(() => component.load()),
    view: () => [
      pageHead("Durable processing", "Jobs", "Every background transition keeps its failure reason and a concrete recovery path."),
      m(".panel", component.loading ? m(".empty", "Loading jobs…") : component.jobs.length ? [
        m("table.table", [
        m("thead", m("tr", ["Job", "Operation", "Resource", "State", "Attempts", "Updated"].map(label => m("th", label)))),
        m("tbody", component.jobs.map(job => m("tr", [
          m("td.mono", job.id), m("td", job.kind), m("td", job.resource_id),
          m("td", m(`span.badge.${job.status}`, job.status)), m("td", String(job.attempts)),
          m("td", new Date(job.updated_at).toLocaleString()),
        ]))),
        ]),
        component.jobs.filter(job => job.error).map(job => m(".job-problem", [
          m("h3", `Job ${job.id} needs attention`),
          m(ProblemView, {problem: job.error!}),
        ])),
      ] : m(".empty", "No background jobs yet.")),
    ],
  }
}
class JobsState {
  jobs: Job[] = []
  loading = true

  async load() {
    try {
      this.jobs = (await api<{items: Job[]}>("/api/v1/jobs?limit=100")).items ?? []
    } finally {
      this.loading = false
      m.redraw()
    }
  }
}

const GraphPage: m.FactoryComponent = () => {
  const component = new GraphState()
  return {
    oninit: () => runTask(() => component.load()),
    view: () => [
      pageHead("Derived memory", "Knowledge graph", "Entities are handles; facts and relations remain traceable to their sources."),
      component.loading ? m(".panel", m(".empty", "Loading graph…")) : m(".split", [
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
    ],
  }
}
class GraphState {
  entities: Entity[] = []
  relations: Relation[] = []
  loading = true

  async load() {
    try {
      const [entities, relations] = await Promise.all([
        api<{items: Entity[]}>("/api/v1/entities?limit=100"),
        api<{items: Relation[]}>("/api/v1/relations?limit=100"),
      ])
      this.entities = entities.items ?? []
      this.relations = relations.items ?? []
    } finally {
      this.loading = false
      m.redraw()
    }
  }
}

const ConflictsPage: m.FactoryComponent = () => {
  const component = new ConflictsState()
  return {
    oninit: () => runTask(() => component.load()),
    view: () => {
      const canResolve = hasCapabilities([capabilities.manageConflicts])
      const headings = ["Conflict", "Fact A", "Fact B", "State", "Resolution"]
      if (canResolve) headings.push("Action")
      return [
      pageHead("Competing claims", "Conflicts", "Manifold keeps both claims visible until an authorized actor resolves them."),
      m(".panel", component.loading ? m(".empty", "Loading conflicts…") : component.conflicts.length ? m("table.table", [
        m("thead", m("tr", headings.map(label => m("th", label)))),
        m("tbody", component.conflicts.map(conflict => m("tr", [
          m("td.mono", conflict.id), m("td.mono", conflict.fact_a_id), m("td.mono", conflict.fact_b_id),
          m("td", m(`span.badge.${conflict.status}`, conflict.status)), m("td", conflict.resolution || "—"),
          canResolve && m("td", conflict.status === "resolved" ? "—" : m(".inline-action", [
            m("input", {
              "aria-label": `Resolution for conflict ${conflict.id}`,
              value: component.resolutions.get(conflict.id) ?? "",
              disabled: component.resolving === conflict.id,
              oninput: (event: InputEvent) => {
                component.resolutions.set(conflict.id, (event.target as HTMLInputElement).value)
              },
              placeholder: "Explicit resolution",
            }),
            m("button.primary", {
              type: "button",
              disabled: component.resolving !== "" || !(component.resolutions.get(conflict.id) ?? "").trim(),
              onclick: () => runTask(() => component.resolve(conflict.id)),
            }, component.resolving === conflict.id ? "Resolving…" : "Resolve"),
          ])),
        ]))),
      ]) : m(".empty", "No competing claims are awaiting review.")),
      ]
    },
  }
}
class ConflictsState {
  conflicts: Conflict[] = []
  resolutions = new Map<string, string>()
  loading = true
  resolving = ""

  async load() {
    try {
      this.conflicts = (await api<{items: Conflict[]}>("/api/v1/conflicts?limit=100")).items ?? []
    } finally {
      this.loading = false
      m.redraw()
    }
  }

  async resolve(id: string) {
    const resolution = (this.resolutions.get(id) ?? "").trim()
    if (!resolution || this.resolving) return
    this.resolving = id
    m.redraw()
    try {
      const resolved = await api<Conflict>(`/api/v1/conflicts/${encodeURIComponent(id)}/resolve`, {
        method: "POST",
        headers: {"Idempotency-Key": idempotencyKey()},
        body: JSON.stringify({resolution}),
      })
      this.conflicts = this.conflicts.map(conflict => conflict.id === id ? resolved : conflict)
      this.resolutions.delete(id)
    } finally {
      this.resolving = ""
      m.redraw()
    }
  }
}

const RenamesPage: m.FactoryComponent = () => {
  const component = new RenameState()
  return {
    oninit: () => { state.problem = null },
    view: () => {
      const blockers = component.plan?.preview.blockers ?? []
      const conflicts = component.plan?.preview.conflicts ?? []
      return [
      pageHead("Identity maintenance", "Cascading rename", "Preview every structured and textual replacement before changing a public semantic ID."),
      m(".split", [
        m(".panel", [
          m(".field", [
            m("label", {for: "rename-resource-type"}, "Resource type"),
            m("select", {
              id: "rename-resource-type",
              value: component.type,
              disabled: component.busy,
              onchange: (event: Event) => {
                component.type = (event.target as HTMLSelectElement).value
                component.plan = null
              },
            }, [
            "folder", "document", "entity", "api_key",
            ].map(value => m("option", {value}, value))),
          ]),
          m(".field", [
            m("label", {for: "rename-from"}, "Current slug"),
            m("input", {
              id: "rename-from",
              value: component.from,
              disabled: component.busy,
              oninput: (event: InputEvent) => {
                component.from = (event.target as HTMLInputElement).value
                component.plan = null
              },
            }),
          ]),
          m(".field", [
            m("label", {for: "rename-to"}, "New slug"),
            m("input", {
              id: "rename-to",
              value: component.to,
              disabled: component.busy,
              oninput: (event: InputEvent) => {
                component.to = (event.target as HTMLInputElement).value
                component.plan = null
              },
            }),
          ]),
          m("button.primary", {
            type: "button",
            disabled: component.busy || !component.from.trim() || !component.to.trim(),
            onclick: () => runTask(() => component.preview()),
          }, component.previewing ? "Building…" : "Build preview"),
        ]),
        m(".panel", component.plan ? [
          m(".eyebrow", `Plan ${component.plan.id}`),
          m("h3", `${component.plan.preview.structured_references} structured references`),
          m("p", `${component.plan.preview.replacements.reduce((sum, item) => sum + item.occurrences, 0)} textual replacements across ${component.plan.preview.replacements.length} documents.`),
          m(".evidence-ledger", component.plan.preview.operations.map(operation =>
            m(".evidence-item", [
              m("strong", operation.resource_type),
              m("p.mono", `${operation.from} → ${operation.to}`),
            ]),
          )),
          component.plan.preview.replacements.length > 0 && m("details", [
            m("summary", "Active document revisions to rewrite"),
            m("ul", component.plan.preview.replacements.map(replacement =>
              m("li.mono", `${replacement.document_id}@${replacement.revision}: ${replacement.occurrences} occurrence(s)`),
            )),
          ]),
          blockers.length > 0 && m(ProblemView, {problem: {
            title: "Unrewritable references",
            detail: blockers.map(item => `${item.resource}${item.location ? ` at ${item.location}` : ""}`).join(", "),
            code: "unrewritable_reference",
            request_id: component.plan.id,
            remediation: {
              summary: "Rewrite or replace every blocking binary document, then build a new preview.",
              steps: blockers.map(item => `${item.resource}: ${item.reason}`),
            },
          }}),
          conflicts.length > 0 && m(ProblemView, {problem: {
            title: "Rename targets are occupied",
            detail: conflicts.map(item => item.resource).join(", "),
            code: "state_conflict",
            request_id: component.plan.id,
            remediation: {
              summary: "Choose free target slugs or preview the complete swap or cycle together.",
              steps: conflicts.map(item => `${item.resource}: ${item.reason}`),
            },
          }}),
          blockers.length === 0 && conflicts.length === 0 && m("button.primary", {
            type: "button",
            disabled: component.busy,
            onclick: () => runTask(() => component.apply()),
          }, component.applying ? "Applying…" : "Apply reviewed plan"),
        ] : m(".empty", "A preview shows every active change before apply.")),
      ]),
      ]
    },
  }
}
class RenameState {
  type = "document"
  from = ""
  to = ""
  plan: RenamePlan | null = null
  previewing = false
  applying = false

  get busy() {
    return this.previewing || this.applying
  }

  async preview() {
    if (this.busy) return
    this.previewing = true
    m.redraw()
    try {
      this.plan = await api<RenamePlan>("/api/v1/rename-plans", {
        method: "POST",
        headers: {"Idempotency-Key": idempotencyKey()},
        body: JSON.stringify({
          operations: [{
            resource_type: this.type,
            from: this.from.trim(),
            to: this.to.trim(),
          }],
        }),
      })
    } finally {
      this.previewing = false
      m.redraw()
    }
  }

  async apply() {
    if (!this.plan || this.busy) return
    const planID = this.plan.id
    this.applying = true
    m.redraw()
    try {
      await api(`/api/v1/rename-plans/${planID}/apply`, {
        method: "POST",
        headers: {"Idempotency-Key": idempotencyKey()},
      })
      m.route.set("/jobs")
    } finally {
      this.applying = false
      m.redraw()
    }
  }
}

const SystemPage: m.FactoryComponent = () => {
  const component = new SystemState()
  return {
    oninit: () => runTask(() => component.load()),
    view: () => [
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
      ] : component.loading ? m(".empty", "Loading component state…") : m(".empty", "Component state is unavailable."),
    ],
  }
}
class SystemState {
  data: {state: string; counts: Record<string, number>; components: Record<string, string>} | null = null
  loading = true

  async load() {
    try {
      const data = await api<{state: string; counts: Record<string, number>; components: Record<string, string>}>("/api/v1/status")
      this.data = data
      state.status = data.state
    } finally {
      this.loading = false
      m.redraw()
    }
  }
}

function applySession(session: UISession): void {
  if (session.csrf_token) {
    state.csrf = session.csrf_token
    sessionStorage.setItem("manifold-csrf", state.csrf)
  }
  state.capabilities = session.capabilities ?? []
  state.authenticated = true
  state.status = "ready"
}

function clearSessionState(): void {
  state.authenticated = false
  state.capabilities = []
  state.status = "checking"
}

function guarded(
  component: m.ComponentTypes,
  required: readonly Capability[],
): m.RouteResolver {
  return {
    async onmatch() {
      if (!state.authenticated) {
        try {
          applySession(await api<UISession>("/api/v1/ui/session"))
        } catch (error) {
          clearSessionState()
          if (isProblem(error) && error.code === "invalid_api_key") {
            state.problem = null
          }
          m.route.set("/login")
          return
        }
      }
      if (!hasCapabilities(required)) {
        state.problem = missingCapabilityProblem(required)
        return AccessDenied
      }
      state.problem = null
      return component
    },
    render: vnode => m(Layout, vnode),
  }
}

m.route.prefix = ""
m.route(document.getElementById("app")!, "/explorer", {
  "/login": Login,
  "/explorer": guarded(Explorer, [capabilities.readDocuments]),
  "/search": guarded(SearchPage, [capabilities.search]),
  "/jobs": guarded(JobsPage, [capabilities.readDocuments]),
  "/graph": guarded(GraphPage, [capabilities.readGraph]),
  "/conflicts": guarded(ConflictsPage, [capabilities.readGraph]),
  "/renames": guarded(RenamesPage, [capabilities.writeDocuments, capabilities.writeGraph]),
  "/system": guarded(SystemPage, [capabilities.readDocuments]),
  "/:404...": {onmatch: () => m.route.set("/explorer")},
})

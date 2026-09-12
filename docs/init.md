# Rolling Star — LSP Gateway 構想と最初の実装スコープ

## 1. これは何か

**Language Intelligence Gateway** である。Editor / Agent から見ると 1 つの Language Server として振る舞い、下流の Language Server 群に対しては LSP Client として振る舞う。目的は、Language Intelligence を Editor や Agent 固有の機能ではなく、**共有 infrastructure として扱えるようにすること**。

この位置づけは最初の実装から外さない。以下で絞るのは「どの機能まで動かすか」であって、「何であるか」ではない。

```
 Neovim ─────── LSP ──────┐
 Claude Code ── LSP ──────┤
 Copilot CLI ── LSP ──────┤──▶ Rolling Star ──▶ Language Server Pool
 Codex ──────── MCP ──────┤                       ├─ actions-languageserver
 Sub Agents ─── LSP/MCP ──┘                       ├─ yaml-language-server
                                                  └─ phpactor ...
```

## 2. Gateway が必要である理由（調査で確定した事実）

**Client 側の routing が貧弱で、Language Server 側の能力が失われている**

- Claude Code は `.lsp.json` の `extensionToLanguage`（拡張子 → languageId）でしか routing できない。path / glob の指定手段がない。さらに同一拡張子を宣言した Server が複数ある場合、**最初に登録された 1 つだけが起動し、残りは起動すらしない**。`.yml` に対して yaml-language-server と actions-languageserver を設定で並立させることは不可能。
- Copilot CLI も `~/.copilot/lsp-config.json` / `.github/lsp.json` の `fileExtensions` で、同じく拡張子単位。
- Codex CLI に built-in LSP はない（v0.125.0 時点）。MCP 経由の community 実装のみ。

**Client 側が custom server request を処理できない**

- actions-languageserver は local reusable workflow の解決に、server → client の custom request `actions/readFile` を使う。VS Code 拡張と nvim-lspconfig はそれぞれ個別に handler を実装しているが、Agent 側にはこの仕組みがない。
- `initializationOptions` にも `sessionToken` と `repos`（`id` / `owner` / `name` / `workspaceUri`）が必要で、repo id は GitHub API から取得する必要がある。
- リポジトリ `actions/languageservices` は contribution を受け付けていない。**上流修正の道は閉じており、Client 側を外から補うしかない**。

これは actions 固有の話ではなく、「Language Server の能力が Client 実装の質に規定される」という構造的問題である。actions は最も分かりやすい例にすぎない。

**既存 OSS では埋まらない**

| OSS | 形 | 不足 |
| --- | --- | --- |
| rassumfrassum / lspx / multi-lsp-proxy | 1 Client → N Servers の aggregation | path-aware routing がない |
| techee/lsp-proxy | primary 1 つ + 他は diagnostics のみマージ | 同上 |
| lspmux (ra-multiplex) | N Clients → 1 Server の共有 | **server → client request を drop する** |

個別要素は存在するが、path-aware routing・custom request 処理・共用 Gateway を一つにまとめたものはない。

## 3. 最初から一般化しておく部分／後から足せる部分

判断基準は**後付けコスト**。中核ループを書き換えることになるものは最初から入れ、加算的に足せるものは後回しにする。

**最初から入れる（後付けすると書き直しになる）**

- Gateway が LSP Server / LSP Client の二面を持つ層構造そのもの
- 設定駆動の **Language Server Definition**（command / args / env / initializationOptions / selector）を N 件持てること
- **Document Routing**：URI → selector（language + glob pattern。LSP の DocumentFilter 相当）で Server を選ぶ。actions を特別扱いしない
- **custom server request の handler table**（`actions/readFile` はその 1 エントリにすぎない）。`workspace/configuration` 等も Gateway 側で応答
- request ID の remap と、Gateway 自身の capability を下流から算出して `initialize` に返すこと

**後から足す（加算的で、上の構造を壊さない）**

- Language Server Pool と Instance 共有（N Clients → 1 Server）／Document View
- MCP Frontend（semantic operation の公開）
- selector が重なったときの Capability Routing / merge

## 4. v1 スコープ

- **Client Session は 1 本**（Agent か Editor か 1 つ）。共有はしない
- **Server は複数起動できる**。ただし **selector は互いに素であること**を前提とし、1 document に 1 Server を選ぶ
- 検証ケース：`.github/workflows/**/*.{yml,yaml}` → actions-languageserver、それ以外の `*.{yml,yaml}` → yaml-language-server。**Claude Code では設定上不可能だった構成が、Gateway 経由で成立する**ことをもって Gateway の価値を示す
- `actions/readFile` handler と initializationOptions の解決（gh CLI 経由）を上記 handler table の実装例として入れる
- Agent 側の設定は `.yml` / `.yaml` → `rolling-star` の 1 件のみ

これで、Gateway の中核（二面構造・selector routing・custom request 処理）はすべて動く。落としているのは共有と merge と MCP frontend だけである。

## 5. v1 に含めない理由

- **Instance 共有**：設計原則は「Client Session が同一の semantic state を観測している場合にのみ共有を許可する」だが、この述語は静的ではない。Editor が buffer を編集した瞬間に View は分岐し、Instance は fork できない。実際に安定する分割は「disk state しか観測しない Client（Agent / Sub Agent / MCP）は共有可、dirty buffer を持つ Editor は専用」という粗い二分になる見込み。ここは Pool の設計時に確定させる
- **Capability merge**：diagnostics は union できるが formatting / rename / codeAction は union 不可。merge policy を決めずに実装すると破綻する。selector を互いに素にしている限り不要
- **MCP Frontend**：Core が固まってから足す

## 6. 未決事項（記録）

- Agent は `didChange` を送らずに disk を直接書き換えるため、Language Server の index が stale になる。LSP では file watching は Client の責務だが Agent は実装していない。Gateway が watcher を持ち `workspace/didChangeWatchedFiles` を合成すべきか
- Sub Agent 増加時の index / memory 重複コストの実測。共有機構への投資判断はこれを見てから
- 名称 `Rolling Star` は JUDY AND MARY の楽曲と検索衝突する

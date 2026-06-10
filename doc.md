# Using Plurality

A guide to what each part of the app does. For installing and hosting it, see
[deployment.md](deployment.md).

## First login

Open the app (web at your server's address, or the desktop/mobile build) and sign in
with a local account or your OpenID provider. The first thing worth doing is opening
the model picker and confirming a model is configured — if the list is empty, the
server has no models yet (see [litellm.md](litellm.md)).

## Chat

Type in the box at the bottom and send. Replies stream in live. You can attach images,
documents, and text snippets to a message — they're uploaded and made available to the
assistant as context. On desktop the layout is a sidebar + conversation; on mobile it
collapses to a bottom nav.

## Models & tools

The **model picker** is more than a dropdown:

- Separate slots for **text**, **vision**, **image generation**, and **image editing** —
  pick a different model for each.
- Three ready-made shortcuts — **Fast**, **Recommended**, and **Smart** — each bundles
  a model and a sensible default tool set, so you can trade speed for quality in one tap.
- Per-tool toggles let you turn individual tools **on**, **off**, or **ask** (require
  approval before each use).

Which models appear, and what each can do (vision, image, audio…), comes from the
server's LiteLLM config.

## Watching what the assistant does

This is the core of Plurality. As the assistant works, each step appears inline:

- **Tool calls** show up as badges with the tool name and a short description filled in
  from the arguments (e.g. the query it's searching for). Tap a badge to open a modal
  with the full arguments and the raw result.
- **File writes** render as **git-style diffs** — created/edited/deleted/moved files
  with color-coded ± lines.
- **File reads** that return a document show up as a downloadable attachment card.
- **Long tasks** show a live **checklist** of sub-steps with done/pending status, plus a
  paused state and reason if the assistant is waiting.
- **Waits** show a **countdown** ticking down to when the assistant will resume.
- **Per-message info**: open the info button on any message to see input/output/total
  tokens, the dollar cost, and which model produced it.
- A **minimap** down the side gives an overview of a long conversation and lets you jump
  around.

## Steering a run

- **Stop**: cancel an in-flight response at any time — it halts the tool loop, not just
  the text.
- **Approve / deny**: when a tool is set to "ask," the run pauses and waits for you to
  approve or reject the specific call before it executes.
- **Switch models** between turns without starting a new conversation.

## Sub-agents (parallel agents)

The assistant can spin off **sub-agents** to work on parts of a task in parallel, or
start entirely new conversations on its own. In the conversation list these are **nested
under their parent**, each showing live status — whether it's typing or which tool it's
currently running — so you can follow a fan-out and drill into any branch.

## Presets / Mini-Apps

A **preset** (Mini-App) is a saved bundle: a custom system prompt plus a model selection
and tool set. Create one in the Presets editor to get a purpose-built assistant — a
coding helper, a research agent, a writing partner — that you can launch in one tap and
pin for quick access. Presets are also what schedules and webhooks run.

## Schedules (cron)

Have the assistant run a prompt on a recurring schedule. In **Schedules**, create a cron
job with a standard cron expression (e.g. `0 9 * * MON` for 9am every Monday) and the
prompt to run. Each run lands as its own conversation, so you can read back exactly what
happened. You can also trigger any schedule manually to test it.

## Webhooks

A **webhook** lets an external system trigger a prompt over HTTP. Create one in
**Webhooks** and you get a URL with a secret token. Hitting that URL (GET or POST) runs
the assistant; the request's method, query, headers, and body are passed in as context,
so the assistant can react to the payload. The token is shown once on creation — use
**rotate** to issue a new one if it leaks. Public webhook traffic is rate-limited per
source IP.

## MCP servers

Plurality speaks the **Model Context Protocol**, so you can plug in external MCP servers
to give the assistant extra tools. Configured servers expose their tools namespaced as
`server__tool`, and start on first use. Configuration lives on the server (see
[deployment.md](deployment.md)).

## Skills

**Skills** are markdown playbooks the assistant can pull in on demand — reusable
instructions for a recurring task. There are server-wide skills shared across users and
per-user skills of your own.

## Memory

Each user has an **important memory** — a short note that's injected into the system
prompt on every conversation. Use it for durable facts about you or how you want the
assistant to behave. Edit it in Account settings, or let the assistant update it itself
when a tool for that is enabled.

## Search

The search box runs a **hybrid search** across all your conversations: keyword
(full-text) and semantic (vector) results merged into a single ranking, so you can find
a past chat by what it was about even if you don't remember the exact words.

## Account & settings

Under **Account** you can manage your important memory, customize appearance/theme,
configure your model shortcuts, change your password, and delete your account (which
removes all of your data).

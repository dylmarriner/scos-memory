# ARIS System Map: What Goes Where

- **Status:** Canonical placement guide for the continuous ARIS runtime
- **Canonical copy:** `dylmarriner/ARIS/docs/ARIS_SYSTEM_MAP.md`
- **Mirrored in:** `ARIS-harness`, `ARIS-intelligence`, `scos-memory` (each at `docs/ARIS_SYSTEM_MAP.md`)
- **Parent document:** [`ARIS_MASTER_TECHNICAL_BLUEPRINT.md`](https://github.com/dylmarriner/ARIS/blob/main/docs/ARIS_MASTER_TECHNICAL_BLUEPRINT.md)
- **Last decided:** 2026-09-28

This page answers one question: **which repository does a piece of ARIS belong in?** It does not replace the master blueprint. Where the two disagree, the master blueprint wins until it is updated. Edit the canonical copy first, then copy it unchanged into the three mirrors in the same change set. The ARIS-harness mirror additionally carries that repository's language switcher line, the Chinese translation `docs/ARIS_SYSTEM_MAP.zh.md` and its consistency record `docs/ARIS_SYSTEM_MAP.i18n.yaml`.

## 1. The shift this map describes

ARIS is not a stateless request/response assistant. The old model was:

```text
user -> build request -> HTTP POST -> model -> response -> destroy execution context
```

ARIS is a continuously running state machine with perception, memory, cognition and action. Continuously maintained state is the centre of the system; inference is something ARIS invokes against that state. A small native model does not need to remember weeks of operation. It needs to reason over a few thousand tokens drawn from a world state that ARIS keeps up to date:

```text
current relevant state + relevant history + current objective + available actions
```

Because ARIS controls the operating system, most perception is semantic (`process.exited`, `unit.failed`, `network.changed`) rather than pixels or audio. Cameras, microphones and screen capture are available when semantic information is not enough.

## 2. The four repositories

| Repository | Owns | Does not own |
| --- | --- | --- |
| `ARIS` | The operating system and product: image, installer, shell, perception adapters, event fabric, host state (tier 1), privileged System Executor, node agent, gateway, packaging, whole-system tests | Goals and task lifecycle, the cognitive world model, model training, durable memory |
| `ARIS-harness` | The Executive: the continuous cognitive loop, goals/tasks/plans, the world model and attention (tier 2), context assembly, routing to models and agents, tool registry, authority sequencing, verification, skills | OS integrations and privileged execution, model weights and training, durable memory storage |
| `ARIS-intelligence` | The native brain and its solver agent: model training, inference, evaluation, releases, plus the solver agent runtime (bounded tool loop, model knowledge store, expert escalation, self-improvement loop) | The ARIS Executive, global goals and task state, privileged OS execution, the durable memory of the user and system |
| `scos-memory` | Durable memory: journal, episodic timeline, semantic memory, provenance, governed promotion, retrieval, consolidation, OKF, memory audit | The Executive, task/tool orchestration, prompt construction, model training |

**No further repository splits are planned.** A new repository is justified only when a component needs an independent release cadence, a different language/runtime, or its own security boundary. Until then new work goes into one of the four as a service or package.

## 3. The continuous runtime pipeline

```text
                         ARIS                                         ARIS-harness
 ┌──────────────────────────────────────────────┐   ┌──────────────────────────────────────────┐
 │ PERCEPTION ADAPTERS                          │   │ TIER 2: WORLD MODEL                      │
 │ procfs systemd journald udev sysfs mounts    │   │ entities, relationships, activities,     │
 │ NetworkManager PipeWire KDE/KWin filesystem  │   │ intentions, confidence, provenance       │
 │ microphone camera screen sensors             │   │                                          │
 │ Home Assistant                               │   │ ATTENTION / SALIENCE                     │
 │        │                                     │   │ goal relevance, urgency, novelty, risk   │
 │        ▼                                     │   │        │                                 │
 │ EVENT FABRIC  (NATS, aris.v1.>)              │   │   matters?──no──▶ update state, continue │
 │ SystemEventEnvelope                          │   │        │yes                              │
 │        │                                     │   │        ▼                                 │
 │        ▼                                     │   │ EXECUTIVE / COGNITIVE LOOP               │
 │ TIER 1: HOST STATE        (aris-eventd)      │   │ orient ▶ retrieve ▶ reason ▶ decide      │
 │ current OS state, raw ring buffers,          │──▶│        │                                 │
 │ cheap filter/coalesce: leave the host?       │   │        ├──▶ speak ──▶ ARIS Shell         │
 └──────────────────────────────────────────────┘   │        ├──▶ act ──▶ policy ▶ ARIS        │
                                                    │        │            System Executor      │
       ARIS-intelligence              scos-memory   │        └──▶ wait / remain silent         │
 ┌───────────────────────────┐ ┌──────────────────┐ └───────┬──────────────────┬───────────────┘
 │ native model inference    │ │ journal          │         │ cognition calls  │ memory calls
 │ /v1/generate              │◀┼──────────────────┼─────────┘                  │
 │ solver agent  /v1/solve   │ │ episodic,        │◀─────────────────────────────┘
 │ training, evals, releases │ │ semantic,        │
 │ model knowledge store     │ │ retrieval,       │
 └───────────────────────────┘ │ promotion        │
                               └──────────────────┘
```

Rules for the pipeline:

1. Most events never reach a model. Tier 1 drops, coalesces or folds events into host state; tier 2 folds them into the world model. Cognition runs only when attention says something matters.
2. **Wait is a first-class outcome.** Observing without responding is correct behaviour, not a failure.
3. Models propose; the Harness Executive authorizes; the ARIS System Executor executes and independently re-validates. Model output never carries authority.
4. Everything that should survive a session is journalled to SCOS Memory and promoted through its governed pipeline.

## 4. Placement table

| Part | Owner | Implemented today | Status |
| --- | --- | --- | --- |
| Linux perception (procfs, systemd, journald, udev, sysfs, mounts, NetworkManager, login sessions) | ARIS | `packages/domain` sources, `services/eventd` | Partial, exercised live |
| KDE/KWin window observation | ARIS | `shell/kwin/aris-window-observer` | Partial |
| Microphone, camera, screen capture | ARIS (perception service publishing `observation.*`) | None | Planned. Speech and vision models used for it are trained and released by ARIS-intelligence |
| Home Assistant and smart-home devices | ARIS (perception and executor adapters) | None | Planned |
| Event fabric | ARIS | `packages/events` (NATS), `SystemEventEnvelope` | Implemented; durable JetStream consumption pending |
| Tier 1 host state, raw ring buffers, cheap filtering | ARIS | `aris-eventd`, `RecentEventBuffer` | Partial |
| Tier 2 world model (belief graph) | ARIS-harness | `BeliefGraph` in `packages/aris/runtime` | In-process only, integration branch |
| Attention and salience | ARIS-harness | `AttentionGate` | In-process only, integration branch |
| Executive, goals, tasks, plans | ARIS-harness | `ARISExecutive` | Integration branch |
| Context assembly for model calls | ARIS-harness | `<ARIS_STATE_CONTEXT>` state packing | Partial |
| Authority, policy, approvals | ARIS-harness (decides), ARIS (re-validates) | `ApprovalPolicyEngine` | Partial |
| Privileged execution | ARIS | `services/system-agent` README only | Planned |
| Speak (voice, notifications, UI) | ARIS Shell | None | Planned |
| Native model inference | ARIS-intelligence | llama.cpp backend, provider `/v1/generate` | Implemented |
| Solver agent (tool loop, model knowledge store, expert escalation) | ARIS-intelligence | `agent/resourceful.py`, `runtime/resourceful.py`, provider `/v1/solve` | Implemented, standalone and provider mode |
| Adaptive test-time compute, deterministic checks, learned escalation router | ARIS-intelligence | `agent/deliberate.py`, `agent/checks.py`, `agent/confidence.py`, `reasoning/candidates.py`, `verifier/` | Implemented |
| Model training, evals, champion/challenger | ARIS-intelligence | `learning/`, `evaluation/`, `benchmarks/`; GPU work on rented vast.ai instances | Implemented, live runs on the 2B development model |
| Durable memory, episodic timeline, retrieval | scos-memory | Go services behind `memory-gateway` | Implemented |
| Skills (procedural learning) | ARIS-harness runs them; ARIS-intelligence evaluates native-model strategies | WikiSkill in both | Partial |

## 5. Decisions recorded on 2026-09-28

### 5.1 World state has two tiers

**Tier 1, ARIS (`aris-eventd`):** raw host state and ring buffers, close to the hardware. It answers "should this event leave the host at all?" with cheap, deterministic filtering and coalescing, and publishes normalized events.

**Tier 2, ARIS-harness:** the cognitive world model and attention. It answers "does this matter to a goal, and should ARIS speak, act or wait?" It holds entities, relationships, activities, intentions, confidence and provenance.

Tier 1 never decides what ARIS should do. Tier 2 never reads hardware directly.

### 5.2 ARIS-intelligence owns the solver agent runtime

The native brain ships with its own bounded agent: the solver agent (implemented as the resourceful agent in `agent/resourceful.py`). It runs a tool loop over non-privileged tools (exact calculator, sandboxed Python, web search and page reading, `command_help` manual lookup and command checking), escalates to an external expert when the native model is out of its depth, verifies answers, and learns from verified results. It keeps a **model knowledge store** of verified facts and strategies used to make the native model better. It runs standalone (`aris-intelligence ask`/`chat`) or as a provider (`POST /v1/solve`).

This does not create a second Executive. The boundary is:

- Harness decides **what** problem to solve and **whether** a result may be acted on. Intelligence decides **how** to solve one delegated problem.
- The solver's tools are non-privileged. Anything that changes the system is returned as a proposal to the Harness Executive, which authorizes it; the ARIS System Executor executes it.
- The model knowledge store is not the durable memory of the user or system. Life history, task history and anything the user expects ARIS to remember belong in SCOS Memory.

### 5.3 Multimodal perception lives in ARIS

Microphone, camera and screen capture are ARIS perception adapters that publish `observation.*` events onto the event fabric like any other sensor. The speech and vision models they use are trained, evaluated and released by ARIS-intelligence and deployed by ARIS.

## 6. Where does my change go?

Ask in order and stop at the first yes:

1. Does it touch hardware, the OS, privileges, packaging or the shell? **ARIS.**
2. Does it decide priorities across time, own goals or tasks, maintain the world model, or authorize actions? **ARIS-harness.**
3. Does it train, run, evaluate or release a model, or solve one delegated problem with non-privileged tools? **ARIS-intelligence.**
4. Must it be remembered durably across sessions, with provenance and governance? **scos-memory.**

If two answers seem to apply, the component is probably two components: split it at the interface, not by copying it into both repositories.

## 7. Known overlaps still to resolve

| Overlap | Repositories | Direction |
| --- | --- | --- |
| Harness calls the native model through interim OpenAI `chat/completions` instead of the ARIS-intelligence provider protocol | ARIS-harness, ARIS-intelligence | Harness adopts `/v1/generate` and `/v1/solve` (protocol v1.0) |
| SCOS exposes `/v1/context/compile`, but Harness owns prompt construction | scos-memory, ARIS-harness | SCOS returns ranked evidence; Harness builds the prompt |
| The solver's model knowledge store and SCOS Memory have no defined exchange | ARIS-intelligence, scos-memory | Define read access to SCOS evidence and a candidate-promotion path from verified solver facts |
| `docs/architecture/overview.md` still describes an in-repo "ARIS Core" orchestrator | ARIS | Rewrite to point at the Harness Executive |
| `aris-eventd` publishes every non-duplicate live event, but its startup `prime()` inventory publishes only escalated events | ARIS, ARIS-harness | Decide how tier 2 gets its startup baseline: publish the full inventory, or let Harness request a snapshot |

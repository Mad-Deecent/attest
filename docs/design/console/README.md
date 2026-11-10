# Admin console: design v2

`mockup.html` is one self-contained file (inline CSS, JS and SVG; no network
requests, no frameworks). Every view has a link (`#decisions/d-1938`, `#policy/door.unlock`, `#setup/1`);
`?theme=dark` and `?still=1` are for screenshots. `⌘K` jumps anywhere; `g` then `p i s a o d v` switches domain; `j`/`k` move through a list.

## Principles

1. Navigation is the domain model. Each domain has one responsibility; anything owned elsewhere is a link, never a copy.
2. An instrument, not a dashboard. Type and hairline rules carry hierarchy; no cards. Each view has one raised surface, its detail pane.
3. Color is information. One accent means *now*. Green, red and amber mean allowed, refused and needs attention, always with a glyph and a word.
4. One concept, one drawing. The level, the belief, the outcome and the chain are each drawn by one function and look the same everywhere.
5. Plain sentences first. Scores and engine reasons live behind "Show the numbers".
6. Say what can never happen ("never possible in the Hallway") where you would act on it.
7. Same anatomy everywhere (rail, list, detail) with one primary action. Keyboard first; on a phone, master–detail becomes push navigation.

## Information architecture

| Domain | Single responsibility | Writes |
|---|---|---|
| **Presence** | Who is here now, per space; how sure, for how long | none |
| **Identities** | People, provider link, roles (read-only), enrollment, what they allowed | invite |
| **Sensors** | Adoption, health, coverage, calibration; **owns spaces** | adopt, place, retire |
| **Agents** | Assistants, tool servers behind the gateway, tools → actions | connect, map |
| **Policy** | Actions, the level each needs, roles × actions, admin-made limits | **every rule** |
| **Decisions** | The ledger: each allow or refuse, explained | none (filter) |
| **Governance** | What is kept, consent history, admins, change audit, retention, export, forget | retention, export, forget |
| Connections | Identity provider, public address, backup, setup | connect |

Departures from the brief. **Spaces belong to Sensors**: for attest, a space is the area some sensors cover.
**Admin-made limits** (a space, a ceiling, an expiry on someone's consent) are rules, so Policy edits them and
Identities only shows them. Consent is *current state* in Identities and *history* in Governance.
**Presence and Decisions are read-only on purpose**: their primary action is selecting a person or a record.

## Signature components

- **Level glyph**: three ascending bars, the top one split in two, because Confirmed means *recognized two
  ways*. The name always sits beside it. It shows a required level (Policy), a held level (Presence, Identities)
  and needed vs had (Decisions).
- **Belief trace**: ten minutes back and four ahead. The line steps up when evidence arrives. It decays so that it
  crosses each threshold exactly when that level lapses (15, 5 or 2 minutes after the freshest supporting evidence).
  A dashed forecast shows what happens "if nothing new arrives". Voice ticks and face dots sit in two lanes on the
  baseline, and a hollow dot means a face was seen that matched nobody. Only the head moves, and it stops under
  `prefers-reduced-motion`.
- **Decision record**: one sentence, then the chain Subject → Evidence → Belief → Consent → Rule. The failing link
  is marked and later links read "Not checked". Expanding it shows the evidence rows and the engine's reasons in mono.
- **Policy matrix**: actions × Keycloak roles, with the level each action needs, edited in place. Every edit
  restates who can *actually* have it done afterwards: they hold the role, they have allowed it, and a space can
  reach the level. Nothing changes until Apply.
- **Coverage map**: spaces × voice and face, with the highest level each space can reach and a sentence for
  each gap. Presence, Policy and Identities all use the same reachability function.

## Vocabulary

| Engine | Console |
|---|---|
| floor / presumed / confident / corroborated | **Anyone / Likely / Sure / Confirmed** |
| identity · producer · agent · zone · permission | person · sensor · agent (assistant) · space · action ("Unlock the front door") |
| delegation, self / admin | "what Tomás has allowed" / "limit set by Noor" |
| ambiguous, conflicted / unknown | "too close to call" / "not recognized", both treated as Anyone |
| posterior, LLR, reasons | only under "Show the numbers" |

## Open questions

1. Is "Confirmed" too strong a word for 99%?
2. The engine expires levels by age; the trace draws a decay that matches those cut-offs. Should the belief API return a "valid until" for each level, so the console never recomputes it?
3. Confirmed needs voice and face *associated* on one subject. At the front door, who submits that association?
4. Should admins see each person's individual choices, or only counts?
5. In a household, should Presence show "not recognized" and "too close to call", or should only the ledger?
6. Forgetting someone stays visibly incomplete until every sensor acknowledges it (as drawn). Is that enough?

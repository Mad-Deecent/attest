# Admin console: design mockup

`mockup.html` is a self-contained, clickable mockup (no network requests, no
frameworks). Open it in a browser. The dashed "Mockup" bar switches each
screen between its normal, empty, pending and problem states, and sets the
theme. Only Tomás's person page and the Home assistant's tools are drawn. Every view has a link: `#home/problem`, `#setup/2/connected`, `#people/tomas`.

## Information architecture

Home · People · Devices · Assistants · Activity · Settings. The first-time
setup wizard (Welcome → Accounts → Your space → People → Done) runs on first
start and can be run again. On a phone, a bottom tab bar holds the first four
sections, and More holds Activity, Settings and Setup.

## Vocabulary

| In code / docs | In the console | Notes |
|---|---|---|
| identity provider, OIDC | Accounts; then the product name ("Keycloak") | the enrolment page already says "account service" |
| identity | person | |
| producer | device (a microphone or camera) | "recognises voices / faces" |
| agent | assistant | as on the enrolment page |
| MCP server | tool server; each entry is a tool | |
| permission | tool, or something an assistant may do | written as a verb phrase: "Unlock the front door" |
| floor permission | Anyone | |
| band `presumed` / `confident` / `corroborated` | Probably / Sure / Certain | Certain always means "recognised two ways" |
| modality | way / kind of recognition (voice, face) | |
| belief, posterior | how sure; shown as a 3-step meter plus the level name | |
| delegation (self-granted) | what *Tomás* has allowed | |
| delegation (admin-made) | set up by you, with its limits shown | read-only for the person, as on the enrolment page |
| zone | room | |
| decision granted / denied | Allowed / Refused, with one plain sentence | |
| tenant / preset | your space; Household / Small office | |

## Decisions that matter

1. **Home leads with one sentence** ("Three people are home, and everything is working."), then who is in each room, then recent decisions and system health. Problems replace the sentence; they don't sit in a banner above a cheerful page.
2. **Four named levels, never percentages, outside Advanced.** The same names appear in the room cards, the tool picker, person pages and the log. The 3-step meter always has the word next to it, so it never relies on colour.
3. **Sensitivity is set per tool with a suggested default.** Adding a tool server is one address plus "Connect". Each discovered tool gets a suggested level that one click changes. Tools that are always allowed (emergency call) are shown as locked.
4. **The console says when a level can never be reached.** Certain needs two kinds of device in the same room. The tool picker, person page ("Without his face, Tomás can't reach Certain") and offline notices say so in plain words, instead of letting a door refuse forever.
5. **Every refusal has a reason a person can act on.** Each log entry is one sentence. Opening one gives a yes/no checklist of each check, and under Advanced the raw LLRs, belief and engine reasons.
6. **The identity provider stays the source of truth.** People and roles are read from it ("add them in Keycloak"). The wizard creates the client and roles there and lists exactly what it created. attest never asks you to edit config.
7. **Same product as `/enrol`.** Same tokens, 44px targets, focus ring, left-border notices and chose/managed split. New: `--warn` (amber), a surface colour, and cards.
8. **Privacy is visible.** Setup, Backup and Remove all repeat that attest never holds voices or faces, and say what each action does to devices.

## Open questions for Brandon

1. Level names: Probably / Sure / Certain? The enrolment page currently shows "at least 95% sure it's you". Should it switch to the names?
2. Can attest really create the client and roles in Keycloak/Auth0/Ory with one admin sign-in, or does setup need a copy-paste fallback for each provider?
3. Device discovery: how does a device announce itself for "Adopt" (mDNS, pairing code)? Adopting would mint its credentials.
4. When the identity provider is unreachable, should decisions keep using cached roles (the mockup proposes 24 hours) or fail closed?
5. Should an admin be able to see what each person allowed, or only counts?
6. Is "Someone new" (unrecognised voice) worth showing on Home, or is it too surveillance-flavoured for a household?
7. Is the per-room presence view acceptable to show to every admin, or should it be off by default in Small office?

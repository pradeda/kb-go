# KB Secret Scanner — Design Spec

**Datum:** 2026-07-13
**Status:** Implementirano (2026-07-13)

## Implementation notes (finalno stanje)

Kanonski skup pravila živi u **`/opt/kb/secret_patterns.json`** (verzija 1) — to je izvor istine, ne tabele ispod. Ključne devijacije od prvobitne skice, sve pokrivene testovima:

- **Provider-prefiks Tier 1** dodat: Anthropic, GitHub (classic + fine-grained), GitLab, Slack, Google, Stripe, HuggingFace, npm, SendGrid, DigitalOcean, AWS, JWT.
- **URL basic-auth** (`scheme://user:pass@`) i **HTTP Basic** (`Authorization: Basic …`) → Tier 1, redakcija samo lozinke (capture grupa).
- **`secret_assignment`** (`PASSWORD=`, `secret=`, `*_token=`) → Tier 1 redakcija vrednosti; **`generic_secret_value`** (`token=`, labavo) → Tier 2 `log` sa entropijskim pragom 4.0.
- **`\b` uklonjen** ispred keyword-a u `arr_api_key`/`secret_assignment`/`generic_secret_value` — inače `POSTGRES_PASSWORD=`/`SONARR_API_KEY=<hex>` ne bi matchovali (`_` je word-char → nema granice). Gotcha, vidi §9.
- **`allow_contains`** (substring needle lista) dodata pored exact `allowlist` — hvata placeholder varijante (`your_password_here`) bez preskakanja pravih parola tipa `MyPassword123`.
- **§4.4 decision pipeline** (dole) formalizovan: match → allowlist → min_len → entropija.

Implementirani fajlovi: `kb-go/secretscan.go` (+ `secretscan_test.go`, 110 testova prolazi), `kb-go/main.go` (`cmdAdd` hook), `/opt/kb/compile.py` (Gate 2), `/opt/kb/secret_patterns.json`. Binary rebuild-ovan i instaliran u `/usr/local/bin/kb`. End-to-end verifikovano: fake secret redigovan u `entries`/FTS/raw, FP (`SONARR_API_KEY`) netaknut.

---

**Status (istorija):** Draft za review
**Povod:** Incident „KB credential redakcija i indeks karantin" (CHANGELOG 2026-07-13) — istorijski OpenRouter/Telegram/RMAB tokeni, qBittorrent credential i SSH ključ nataloženi u pretraživom KB sadržaju kroz automatizovane upise.

## 1. Cilj

Sprečiti da tajne (tokeni, API ključevi, kredencijali) uđu u pretraživ KB sadržaj — automatski, bez reda za pregled i bez ručne intervencije. Kad regex okine, sadržaj se **sanitizuje u letu** (tipizirani placeholder) i nastavlja kroz pipeline; original secret nikad ne dospe u `entries`/FTS/Chroma.

### Ne-ciljevi (YAGNI)

- ❌ Karantin-red / `kb quarantine` review queue
- ❌ Telegram / dashboard notifikacije
- ❌ Detekcija visoke entropije (previše false-positive-a za auto-redakciju)
- ❌ Retroaktivni sken postojećeg korpusa (incident je to već očistio; ovo je *prevencija* ubuduće)

## 2. Zašto ovaj sloj (arhitektonski nalaz)

`kb add` (`kb-go/main.go:cmdAdd`) upiše ceo `content` u SQLite `entries` **odmah** (`INSERT`, main.go:290), pre bilo kakvog compile-a. `compile.py` (`get_uncompiled`/`get_unembedded`, compile.py:60–76) embed-uje **iz `entries.content`**, ne iz raw `.md` fajla. Raw fajl je arhivska/metadata kopija.

**Zaključak:** pretraživ izvor je `entries.content`. Skener mora da radi nad tim stringom pre `INSERT`-a. Menjanje raw fajla ništa ne čisti.

## 3. Arhitektura

Dve tačke primene, **jedan zajednički izvor pravila**:

```
┌─────────────┐   sanitize()    ┌──────────┐
│ kb add /    │ ──────────────► │ entries  │ ──► FTS + Chroma
│ MCP add     │  (pre INSERT)   │ .content │
└─────────────┘                 └──────────┘
       ▲                              ▲
       │ secret_patterns.json         │ safety-net sken pre embed
       │ (single source of truth)     │ (compile.py — hvata rsync/raw drop)
       └──────────────┬───────────────┘
                      │
            /opt/kb/secret_patterns.json
```

### 3.1 Primarni gate — `kb-go/main.go` (write time)
- Nova funkcija `sanitize(content string) (clean string, hits []Hit)` poziva se u `cmdAdd` **pre** `INSERT`-a.
- Skenira `content`; za svaki pogodak zameni matched vrednost tipiziranim placeholderom.
- Upiše *očišćen* `content` i u `entries` i u raw fajl (frontmatter build koristi već očišćen string).
- Ako ima pogodaka: backup originala + jedna linija u log (§6).

### 3.2 Safety-net — `/opt/kb/compile.py` (embed time)
- Isti regex nad `entries.content` u `get_unembedded()` petlji, pre `embed_entries()`.
- Hvata izvore koji zaobiđu `kb add`: direktan raw drop u `/opt/kb/raw/`, rsync sa NAS-a, bilo koji budući ingest.
- Pogodak → `UPDATE entries SET content=<clean>`, prepiši raw fajl, pa embed. Backup + log kao gore.

### 3.3 Zajednički izvor pravila — `/opt/kb/secret_patterns.json`
Jedan JSON fajl koji čitaju i Go i Python → nema drift-a između dva sloja. Shema u §4.3.

## 4. Regex set (srž ovog spec-a)

Dva tira po pouzdanosti + allowlist. Svaki pattern nosi svoju akciju (`redact` | `log`).

### 4.1 Tier 1 — value-shaped, high-confidence → `redact`
Hvataju **oblik same vrednosti**, ne reč. FP iz incidenta (`KEY`, `SONARR_API_KEY`) su bare-word i ove regexe **ne okidaju**.

| Naziv | Regex | Placeholder |
|-------|-------|-------------|
| `openrouter_key` | `sk-or-v1-[0-9a-f]{64}` | `<REDACTED_OPENROUTER_KEY>` |
| `openai_key` | `sk-[A-Za-z0-9]{20,}` | `<REDACTED_OPENAI_KEY>` |
| `telegram_bot_token` | `\b[0-9]{8,10}:[A-Za-z0-9_-]{35}\b` | `<REDACTED_TELEGRAM_TOKEN>` |
| `ssh_private_key` | `-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----[\s\S]*?-----END (?:[A-Z]+ )?PRIVATE KEY-----` | `<REDACTED_SSH_PRIVATE_KEY>` |
| `jwt` | `eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}` | `<REDACTED_JWT>` |
| `arr_api_key` | `(?i)\bapi[_-]?key["'\s:=]+([0-9a-f]{32})\b` (redaktuje samo grupu 1) | `<REDACTED_API_KEY>` |
| `aws_access_key` | `\b(?:AKIA\|ASIA)[0-9A-Z]{16}\b` | `<REDACTED_AWS_KEY>` |

Napomene:
- `arr_api_key` hvata 32-hex **samo posle** `api_key`/`apikey` konteksta → ne redaktuje slučajne md5 hash-eve u sadržaju. Pokriva Sonarr/Radarr/Prowlarr/Kavita/Kapowarr ključeve.
- `openai_key` namerno širi (`sk-` + 20+) da uhvati i buduće varijante; `openrouter_key` je uži i ide prvi (redosled bitан — specifičniji pre generičkog).

### 4.2 Tier 2 — keyword-proximity, lower-confidence → `log` (default)
Kredencijali bez fiksnog oblika (proizvoljne lozinke). Value-shaped ne radi → oslanjamo se na blizinu ključne reči. **Default akcija `log`, ne `redact`**, jer je FP rizik veći; korisnik može prebaciti pojedinačni pattern na `redact` posle perioda posmatranja.

| Naziv | Regex (skica) | Akcija |
|-------|---------------|--------|
| `qbittorrent_cred` | `(?i)qbittorrent[\s\S]{0,40}?(?:pass\|password\|pwd)["'\s:=]+(\S+)` | `log` |
| `generic_password_assign` | `(?i)\b(?:password\|passwd\|pwd)["'\s:=]+(\S{6,})` | `log` |
| `rmab_token` | *(TBD — RMAB/readmeabook token format nepoznat; popuniti kad se potvrdi uzorak)* | `log` |

> Tier 2 se namerno drži konzervativno. Ako u praksi okine na pravu tajnu, prebacimo taj pattern na `redact` sa capture-group redakcijom (redaktuje samo grupu 1, ostavlja kontekst).

### 4.3 Shema `secret_patterns.json`
```json
{
  "version": 1,
  "patterns": [
    {
      "name": "openrouter_key",
      "regex": "sk-or-v1-[0-9a-f]{64}",
      "placeholder": "<REDACTED_OPENROUTER_KEY>",
      "action": "redact",
      "capture_group": 0
    }
  ],
  "allowlist": [
    "SONARR_API_KEY", "RADARR_API_KEY", "PROWLARR_API_KEY",
    "KEY", "API_KEY", "your-api-key-here", "example",
    "<REDACTED_", "sk-or-v1-xxxx"
  ]
}
```
- `capture_group`: 0 = redaktuj ceo match; N = redaktuj samo tu grupu (za kontekstualne patterne).
- `allowlist`: ako se matched substring (case-insensitive) nalazi u allowlist-i ili je već `<REDACTED_…>` placeholder → preskoči (bez re-redakcije, bez log-a). Direktno gasi FP iz incidenta.

## 5. Redakcija — ponašanje

- Zamena je **in-place** u `content` stringu: matched vrednost → placeholder iz patterna.
- Idempotentno: već-redigovan `<REDACTED_…>` se ne dira (allowlist prefix `<REDACTED_`).
- Više pogodaka u istom sadržaju → svi redigovani u jednom prolazu.
- Redosled patterna je deterministički (kako su u JSON-u); specifičniji pre generičkih.

## 6. Backup + trag (bez notifikacije)

- **Backup originala** pre mutacije → `/opt/kb/quarantine/<id>-<timestamp>.orig` (`0600`, dir `0700`). Reverzibilnost.
- **Jedna append linija** → `/opt/kb/quarantine.log`:
  `2026-07-13T14:22:01 id=441 source=cli hits=openrouter_key,telegram_bot_token action=redact`
- Bez Telegrama/dashboarda. „Notifikacija" = trag u logu + (za `kb add`) poruka na stderr:
  `kb add: redigovano 2 tajne pre upisa (openrouter_key, telegram_bot_token). Original: /opt/kb/quarantine/441-...orig`

## 7. False-positive strategija

1. **Value-shaped pre bare-word** — Tier 1 hvata oblike vrednosti, ne reči.
2. **Allowlist** — bare-word literali (`KEY`, `SONARR_API_KEY`) i placeholder oznake.
3. **Tier 2 = `log` default** — sumnjive kontekstualne stvari se ne mutiraju dok se ne potvrde.
4. **Backup + reverzibilnost** — čak i pri FP redakciji original je u `quarantine/`.

## 8. Testiranje

- **Go unit** (`main_test.go`): `sanitize()` nad fixture-ima — svaki Tier 1 pattern pozitivan; allowlist negativni (`SONARR_API_KEY`, `KEY`); idempotencija (redigovan ostaje isti); multi-hit; capture-group redakcija.
- **Python test** (`/opt/kb` test): safety-net sken nad `entries.content` fixture-om; raw drop scenario; parity — isti ulaz daje isti izlaz kao Go (zajednički JSON).
- **Regresija incidenta**: fixture sa tačnim oblicima koji su curili (OpenRouter `sk-or-v1-…`, Telegram `\d+:AA…`) → redigovani; `SONARR_API_KEY` literal → netaknut.

## 4.4 Verifikacija pogotka (decision pipeline)

Za svaki match: `allowlist/allow_contains → min_len → (ako confidence low i min_entropy>0) Shannon entropija`. `confidence: high` preskače entropiju (struktura je dokaz). `action: redact` mutira sadržaj; `action: log` samo beleži hit. Entropija se računa nad bajtovima (parity Go↔Python, ASCII secreti identični).

## 9. Otvorena pitanja / rešeno

1. **MCP `add` put** — REŠENO: `/opt/kb/mcp_server.py:49` shell-uje `/usr/local/bin/kb add`, pa ga Gate 1 (§3.1) pokriva u potpunosti.
2. **RMAB/readmeabook token format** — i dalje nepoznat; hvata ga `generic_secret_value` (`log`) preko entropije dok se ne potvrdi uzorak i doda namenski prefiks pattern.
3. **Gotcha `\b`** — word-boundary ispred secret-keyword-a NE matchuje prefiksovane varijante (`FOO_PASSWORD`, `SONARR_API_KEY`) jer je `_` word-char. Rešeno uklanjanjem `\b`; regresija pokrivena testom `TestSanitize_Tier1Redacts/password_assign`.
4. **RE2 paritet** — Go `regexp` (RE2) nema backreference/lookaround; svi patterni u `secret_patterns.json` su u RE2 podskupu koji Python `re` takođe prihvata → identičan rezultat (potvrđeno parity testom).

## 10. Fajlovi koji se menjaju

- `kb-go/main.go` — `sanitize()`, poziv u `cmdAdd`, backup+log helperi
- `kb-go/main_test.go` — testovi
- `/opt/kb/compile.py` — safety-net sken u embed petlji
- `/opt/kb/secret_patterns.json` — **novo**, zajednička pravila
- `/opt/kb/quarantine/` + `/opt/kb/quarantine.log` — **novo**, runtime artefakti

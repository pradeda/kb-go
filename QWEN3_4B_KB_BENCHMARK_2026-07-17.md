# Qwen3 4B KB synthesis benchmark — 2026-07-17

## Cilj

Proveriti da li lokalni `qwen3:4b` na Nexus CPU-u može da zameni trenutni
OpenRouter `google/gemini-2.5-flash-lite` samo u završnoj sintezi rezultata koje
već vraća postojeći `/kb/search` pipeline.

Produkcioni KB provider nije menjan. Ollama kontejner je podignut sa 4 GB na
5 GB limita i model `qwen3:4b` (2,5 GB na disku) je preuzet.

## Metod

- Isti `/kb/search` endpoint, `format=full`, rezultat rerankinga i wiki indeks.
- Isti produkcioni prompt iz `main.go`, uključujući do pet rezultata i do 3.000
  znakova sadržaja po rezultatu.
- Lokalno: Ollama `qwen3:4b`, CPU-only, 8.192 tokena konteksta, temperatura 0,
  najviše 700 izlaznih tokena, `think:false` i `/no_think`.
- OpenRouter: stvarna produkciona komanda `kb ask`, bez izmene konfiguracije.

## Rezultati

### Isti glavni upit

Upit: „Koji je trenutni tok objavljivanja RSS članaka u YouTube AI Ingest
projektu i koje su bezbednosne granice?“

| Metrika | Lokalni Qwen3 4B | OpenRouter Flash Lite |
|---|---:|---:|
| KB retrieval | 1,62 s | 3,90 s |
| LLM sinteza | 433,27 s | 6,29 s |
| Ukupno | 434,89 s | 10,20 s |
| Ulaz lokalnog modela | 2.861 token | nije izloženo kroz CLI |
| Izlaz lokalnog modela | 700 tokena, dosegnut limit | kompletan odgovor |
| Lokalna brzina izlaza | 3,04 tokena/s | nije izloženo kroz CLI |
| Učitavanje lokalnog modela | 2,30 s | n/a |

Lokalna sinteza je na ovom upitu bila oko 69 puta sporija, a ceo tok oko 43
puta sporiji. Najveći lokalni trošak nije učitavanje modela, već obrada punog
KB konteksta (200,49 s) i generisanje (230,08 s).

### OpenRouter kontrolni upiti

| Upit | Ukupno | Ishod |
|---|---:|---|
| Article orphan root cause i rešenje | 5,90 s | detaljan odgovor sa KB izvorom |
| Nepostojeći podatak o boji rack-a | 6,92 s | ispravno prijavljeno da podatak ne postoji |

Medijana tri izmerena OpenRouter poziva je 6,92 s ukupno.

## Resursi i kvalitet

- Ollama je tokom rada koristio oko 4,03 GiB od limita 5 GiB i približno svih
  šest CPU jezgara (oko 550–580% Docker CPU).
- Nije bilo OOM-a, restarta kontejnera niti dodatnog rasta swap zauzeća.
- Limit od 5 GB je dovoljan; prethodnih 4 GB bi ostavilo praktično nultu rezervu.
- Aktuelni Ollama artefakt se identifikuje kao `Qwen3 4B Thinking 2507`.
  Uprkos `think:false` i `/no_think`, odgovor je emitovao meta-rezonovanje na
  engleskom, zatim pokušao odgovor i bio presečen usred rečenice na 700 tokena.
- OpenRouter odgovor na isti upit bio je kompletan, fokusiran i na srpskom.

## Zaključak

Ovaj konkretni `qwen3:4b` nije prikladna zamena za stalni interaktivni `kb ask`
na Nexus i5-8500T CPU-u. Memorijski staje u 5 GB, ali latencija i ponašanje
thinking-only artefakta čine ga znatno lošijim od trenutnog Flash Lite toka.
Model ostaje preuzet radi daljih eksperimenata, ali nije povezan sa produkcionim
`kb ask`; posle testa je izbačen iz RAM-a.

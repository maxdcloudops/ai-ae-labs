| Сценарій | Рівень межі | Вердикт | Що побачить модель |
|---|---|---|---|
| `malformed-json` | рівень JSON: текст обірваний | **rejected** | tool arguments are not valid for this contract: unexpected EOF |
| `wrong-type` | рівень схеми: число там, де рядок | **rejected** | tool arguments are not valid for this contract: json: cannot unmarshal number into Go struct field RateInput.… |
| `unknown-field` | рівень схеми: поле, якого немає в контракті | **rejected** | tool arguments are not valid for this contract: json: unknown field "currency" |
| `missing-field` | рівень схеми: обов'язкове поле відсутнє | **rejected** | arguments cannot be repaired safely: field "target": invalid currency code: currency code is empty (expected … |
| `bad-code-shape` | рівень домену: валідний JSON, невалідний код | **rejected** | arguments cannot be repaired safely: field "base": invalid currency code: "US1" must be three LATIN letters (… |
| `unknown-currency` | рівень домену: форма правильна, валюти немає | **rejected** | unknown currency code: XQZ is not published by this provider |
| `lowercase` | нормалізація: регістр — не помилка | **accepted** | пройшов усі рівні: base=USD target=UAH history_days=0 |
| `alias` | ремонт: однозначний синонім валюти | **repaired** | base: resolved alias "євро" → "EUR" → base=EUR target=UAH history_days=0 |
| `ambiguous-alias` | домен: «долар» — їх багато, ремонт НЕБЕЗПЕЧНИЙ | **rejected** | arguments cannot be repaired safely: field "base": invalid currency code: "долар" must be three letters (ISO … |
| `range-overflow` | ремонт: діапазон підрізається до стелі | **repaired** | history_days: clamped 365 → 14 → base=USD target=UAH history_days=14 |
| `happy-path` | контроль: валідний виклик мусить пройти | **accepted** | пройшов усі рівні: base=USD target=UAH history_days=2 |

Артефакти: results/faults.json, results/faults.svg

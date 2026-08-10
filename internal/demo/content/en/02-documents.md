key: documents
title: Q3 report — key figures
model: claude-opus-4-7
docs: quarterly-report-q3.pdf:18, product-roadmap.md:6
---
@user
Summarise the key figures of the attached report and list the three biggest
risks for Q4.

@assistant
**Key figures Q3**

| Metric              | Q3      | vs. Q2  |
| ------------------- | ------- | ------- |
| Revenue             | 4.28 M€ | +11.4 % |
| Gross margin        | 62.1 %  | +1.8 pp |
| Active tenants      | 1 940   | +214    |
| Churn (monthly)     | 1.3 %   | −0.2 pp |

The growth is carried almost entirely by the self-hosted tier; the managed
offering is flat quarter over quarter *(quarterly-report-q3.pdf, section 2.3)*.

**Top risks for Q4**

1. **Single-supplier dependency** — 71 % of inference volume runs through one
   region. The roadmap schedules a second region for November
   *(product-roadmap.md, "Resilience")*.
2. **Support load per tenant** grew faster than the tenant count (+38 % tickets
   vs. +12 % tenants).
3. **Renewals** — 22 % of annual contracts expire in December; the report flags
   nine accounts without a renewal conversation yet.

Ask me for any of these sections in more detail — both documents stay attached
to this chat.

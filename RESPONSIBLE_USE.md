# Responsible Use

> **This is not legal advice.** The legal references below are *design drivers*,
> not an authoritative statement of your obligations. Laws vary by jurisdiction
> and change over time. **Consult qualified legal counsel** before operating this
> tool, especially where it may encounter illegal content.

vismod is a public good for trust & safety. It may encounter illegal content —
in particular **child sexual abuse material (CSAM)**. A careless deployment
causes real-world harm. Treat the rules in this document as **acceptance
criteria, not suggestions**.

---

## 1. vismod has NO CSAM detection in v1

The v1 pipeline performs **classifier-based moderation only**. It does **not**
detect CSAM.

- The CSAM control is a **perceptual-hash match** against known-bad lists (Meta
  PDQ for images, TMK+PDQF / vPDQ for video). That matcher is **v1.1**. v1 ships
  only the *seam*: the `CSAM_HASH_MATCH` category, the `match_type`/`match_list`
  schema fields, and a no-op matcher.
- **Operators who need CSAM coverage cannot rely on this tool until the v1.1
  matcher ships.** Do not represent v1 as providing CSAM detection.

## 2. Potential-CSAM handling (the safe failure mode)

The generic `SEXUAL` classifier **will sometimes fire on content that is
actually CSAM**. A high-severity `SEXUAL` result is **not** a CSAM
determination — but it **must be handled as potential-CSAM**.

- **Trigger:** a `SEXUAL` category result with normalized
  `Score >= thresholds.SEXUAL.potential_csam` (config key; default `0.667`,
  i.e. Azure severity level 4).
- **What vismod does:** before the result is written to the Sink, the frame is
  **diverted to a human-review channel**. The diverted record carries only
  `SHA-256(frame)` + metadata — **never the frame bytes and never provider
  `Raw` text** (§G.2). The default `LogDiverter` emits a structured WARN event;
  a production deployment must supply a `Diverter` that writes to an **encrypted,
  access-controlled** review queue.
- **What vismod must NOT do:** no fully-automated consequential action on a
  positive or potential match. Borderline / low-confidence results go to a
  **human** (§4 below).

## 3. Never persist or transmit the illegal media itself

- vismod operates on **hashes and derivatives** plus **per-job transient working
  copies** that are deleted promptly (the frame `WorkDir` is created, owned, and
  removed per job).
- The audit log stores `SHA-256(Raw)` + model identity + verdict, **never the
  media or the provider's free-text/OCR/caption output**.
- Any transiently held flagged material must be **encrypted at rest** under
  **strict access control**.
- Originals leave the system **only via a lawful channel** (see §5). Durable
  queue payloads (Redis/asynq, M5) and any operator UI (`asynqmon`) must carry
  **opaque IDs/refs, never media bytes**, and must be access-controlled.

## 4. Human-in-the-loop

No fully-automated consequential action follows a positive or potential match.
Positive, borderline, and low-confidence results route to **human review**. The
precision/recall tradeoff is tunable via the per-category thresholds — see
[MODEL_AND_HASH_LIMITATIONS.md](MODEL_AND_HASH_LIMITATIONS.md).

## 5. Reporting guidance (US example — not legal advice)

In the United States, providers report apparent CSAM to the **NCMEC
CyberTipline** (`report.cybertip.org`). Statutory references that shaped this
design include **18 U.S.C. § 2258A** and the **REPORT Act**. These are **design
drivers, not legal advice** — your reporting obligations depend on your
jurisdiction, your role, and the facts. Non-US regimes (EU CSA Regulation, UK
Online Safety Act, IWF/INHOPE channels) are **noted but not encoded** in v1.
**Consult counsel** to determine your obligations and the correct channel.

## 6. Do NOT test against real CSAM

**Never** feed real or suspected CSAM into this tool to "try it out." Doing so is
illegal in most jurisdictions and re-victimizes survivors. Test with synthetic,
benign, or clearly-legal material. Per Microsoft policy, the Azure AI Content
Safety adapter **must not be used to detect CSAM** — that is the hash-match
seam's job (v1.1), not the classifier's.

## 7. Transparency on limits

Moderation models produce false positives and exhibit bias; perceptual hashes
are evadable. Operators must understand these limits before acting on output —
see [MODEL_AND_HASH_LIMITATIONS.md](MODEL_AND_HASH_LIMITATIONS.md). Scores are
**within-provider comparable only**; a threshold tuned for one adapter is **not
portable** to another.

---

*ROOST positioning:* ROOST (Robust Open Online Safety Tools) is a 501(c)(3)
launched in February 2025 to make trust & safety infrastructure open, shared,
and auditable. vismod is the **Classification** stage feeding a review console /
rules engine downstream — it is one component of a safety system, not a complete
one.

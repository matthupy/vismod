package hive

import "github.com/matthupy/vismod/pkg/moderation"

// This file is the heart of the Hive normalization challenge. Hive's visual
// moderation model is a bank of independent "heads" (sub-classifiers), each
// covering one content concept (a gun, nudity, smoking, ...). The sync API
// flattens every head's classes into ONE list of {class, score} objects, with
// no head grouping in the wire format. Per head, the confidence scores of its
// positive classes plus its single negative class sum to 1.
//
// To normalize we must reconstruct that structure from a static table: which
// head each class belongs to, whether it is a positive or negative signal, and
// which canonical category a positive signal maps to. See normalize.go for how
// these fields drive the per-head-sum / cross-head-max reduction.
//
// Source: docs.thehive.ai visual moderation class descriptions (heads grouped
// by Sexual / Violence-Weapons-Gore / Drugs-Smoking / Hate-Bullying / Other).

type polarity int

const (
	// negative is a head's "safe complement" class (no_gun, general_not_*). It is
	// structural — it exists only so the head's scores sum to 1 — and is dropped
	// during normalization. It is NOT a harm label and is never emitted.
	negative polarity = iota
	// positive is a harm/attribute-present signal whose score maps to a category.
	positive
)

// catSkip is a private sentinel marking a positive class whose head is purely
// descriptive (image type, text presence, QR code, ...). These carry no harm
// meaning, so they are deliberately not emitted as harm CategoryResults. This is
// a documented provenance decision, parallel to how MEDICAL/SPOOF provenance is
// documented in pkg/moderation — not a silent drop of a harm signal.
const catSkip moderation.Category = "__skip__"

// classInfo records the head membership, polarity, and canonical mapping of one
// Hive class string.
type classInfo struct {
	head string
	pol  polarity
	cat  moderation.Category // meaningful only when pol == positive
}

// pos is a positive-class table entry constructor.
func pos(head string, cat moderation.Category) classInfo {
	return classInfo{head: head, pol: positive, cat: cat}
}

// neg is a negative-class (safe complement) table entry constructor. The head is
// kept for documentation/debugging; the category is unused.
func neg(head string) classInfo { return classInfo{head: head, pol: negative} }

// skip is a descriptive positive-class table entry constructor.
func skip(head string) classInfo { return classInfo{head: head, pol: positive, cat: catSkip} }

// classTaxonomy maps every known Hive visual-moderation class to its structure.
// A class absent from this table is treated as an unknown future signal and
// falls back to OTHER (raw label preserved, score carried) — never dropped.
var classTaxonomy = map[string]classInfo{
	// --- Sexual content heads -------------------------------------------------
	// NSFW head splits across two canonical categories by class.
	"general_nsfw":                    pos("nsfw", moderation.CategorySexual),
	"general_suggestive":              pos("nsfw", moderation.CategorySuggestiveRacy),
	"general_not_nsfw_not_suggestive": neg("nsfw"),

	"yes_sexual_activity": pos("sexual_activity", moderation.CategorySexual),
	"no_sexual_activity":  neg("sexual_activity"),
	"yes_realistic_nsfw":  pos("realistic_nsfw", moderation.CategorySexual),
	"no_realistic_nsfw":   neg("realistic_nsfw"),
	"yes_sexual_intent":   pos("sexual_intent", moderation.CategorySexual),
	"no_sexual_intent":    neg("sexual_intent"),
	"yes_undressed":       pos("undressed", moderation.CategorySexual),
	"no_undressed":        neg("undressed"),
	"yes_sex_toy":         pos("sex_toy", moderation.CategorySexual),
	"no_sex_toy":          neg("sex_toy"),
	"yes_female_nudity":   pos("female_nudity", moderation.CategorySexual),
	"no_female_nudity":    neg("female_nudity"),
	"yes_male_nudity":     pos("male_nudity", moderation.CategorySexual),
	"no_male_nudity":      neg("male_nudity"),
	"yes_genitals":        pos("genitals", moderation.CategorySexual),
	"no_genitals":         neg("genitals"),
	"yes_breast":          pos("breast", moderation.CategorySexual),
	"no_breast":           neg("breast"),
	"yes_butt":            pos("butt", moderation.CategorySexual),
	"no_butt":             neg("butt"),
	"yes_bulge":           pos("bulge", moderation.CategorySexual),
	"no_bulge":            neg("bulge"),
	"kissing":             pos("tongue", moderation.CategorySexual),
	"licking":             pos("tongue", moderation.CategorySexual),
	"no_tongue":           neg("tongue"),

	"animal_genitalia_and_human": pos("animal_genitalia", moderation.CategorySexual),
	"animal_genitalia_only":      pos("animal_genitalia", moderation.CategorySexual),
	"animated_animal_genitalia":  pos("animal_genitalia", moderation.CategorySexual),
	"no_animal_genitalia":        neg("animal_genitalia"),

	// Suggestive / racy (clothed, attire-based) heads.
	"yes_female_underwear":   pos("female_underwear", moderation.CategorySuggestiveRacy),
	"no_female_underwear":    neg("female_underwear"),
	"yes_male_underwear":     pos("male_underwear", moderation.CategorySuggestiveRacy),
	"no_male_underwear":      neg("male_underwear"),
	"yes_bra":                pos("bra", moderation.CategorySuggestiveRacy),
	"no_bra":                 neg("bra"),
	"yes_panties":            pos("panties", moderation.CategorySuggestiveRacy),
	"no_panties":             neg("panties"),
	"yes_negligee":           pos("negligee", moderation.CategorySuggestiveRacy),
	"no_negligee":            neg("negligee"),
	"yes_cleavage":           pos("cleavage", moderation.CategorySuggestiveRacy),
	"no_cleavage":            neg("cleavage"),
	"yes_female_swimwear":    pos("female_swimwear", moderation.CategorySuggestiveRacy),
	"no_female_swimwear":     neg("female_swimwear"),
	"yes_male_shirtless":     pos("male_shirtless", moderation.CategorySuggestiveRacy),
	"no_male_shirtless":      neg("male_shirtless"),
	"yes_bodysuit":           pos("bodysuit", moderation.CategorySuggestiveRacy),
	"no_bodysuit":            neg("bodysuit"),
	"yes_miniskirt":          pos("miniskirt", moderation.CategorySuggestiveRacy),
	"no_miniskirt":           neg("miniskirt"),
	"yes_sports_bra":         pos("sports_bra", moderation.CategorySuggestiveRacy),
	"no_sports_bra":          neg("sports_bra"),
	"yes_sportswear_bottoms": pos("sportswear_bottoms", moderation.CategorySuggestiveRacy),
	"no_sportswear_bottoms":  neg("sportswear_bottoms"),

	// --- Violence / Weapons / Gore heads -------------------------------------
	"gun_in_hand":     pos("gun", moderation.CategoryWeapons),
	"gun_not_in_hand": pos("gun", moderation.CategoryWeapons),
	"animated_gun":    pos("gun", moderation.CategoryWeapons),
	"no_gun":          neg("gun"),

	"knife_in_hand":              pos("knife", moderation.CategoryWeapons),
	"knife_not_in_hand":          pos("knife", moderation.CategoryWeapons),
	"culinary_knife_in_hand":     pos("knife", moderation.CategoryWeapons),
	"culinary_knife_not_in_hand": pos("knife", moderation.CategoryWeapons),
	"no_knife":                   neg("knife"),

	"very_bloody":     pos("blood", moderation.CategoryGoreGraphic),
	"a_little_bloody": pos("blood", moderation.CategoryGoreGraphic),
	"other_blood":     pos("blood", moderation.CategoryGoreGraphic),
	"no_blood":        neg("blood"),

	"human_corpse":    pos("corpse", moderation.CategoryGoreGraphic),
	"animated_corpse": pos("corpse", moderation.CategoryGoreGraphic),
	"no_corpse":       neg("corpse"),

	"yes_fight":        pos("fight", moderation.CategoryViolence),
	"no_fight":         neg("fight"),
	"yes_animal_abuse": pos("animal_abuse", moderation.CategoryViolence),
	"no_animal_abuse":  neg("animal_abuse"),

	"hanging":             pos("hanging", moderation.CategorySelfHarm),
	"noose":               pos("hanging", moderation.CategorySelfHarm),
	"no_hanging_no_noose": neg("hanging"),
	"yes_self_harm":       pos("self_harm", moderation.CategorySelfHarm),
	"no_self_harm":        neg("self_harm"),
	"yes_emaciated_body":  pos("emaciated", moderation.CategorySelfHarm),
	"no_emaciated_body":   neg("emaciated"),

	// Child-safety is a high-consequence harm flag but the canonical taxonomy has
	// no dedicated category for it yet (adding one is a cross-adapter contract
	// change, out of scope here). Intentionally mapped to OTHER: it is preserved
	// with the head's label, and because OTHER heads are emitted per-head (never
	// max-collapsed, see normalize.go stage 2) it can never be dropped by a louder
	// OTHER signal such as gambling. Hash-based CSAM divert (M4) is a separate
	// pre-stage on CSAM_HASH_MATCH and is unaffected by this classifier mapping.
	"yes_child_safety": pos("child_safety", moderation.CategoryOther),
	"no_child_safety":  neg("child_safety"),

	// --- Drugs / Smoking / Vices heads ---------------------------------------
	"yes_pills":            pos("pills", moderation.CategoryDrugs),
	"no_pills":             neg("pills"),
	"illicit_injectables":  pos("injectables", moderation.CategoryDrugs),
	"medical_injectables":  pos("injectables", moderation.CategoryMedical),
	"no_injectables":       neg("injectables"),
	"yes_smoking":          pos("smoking", moderation.CategoryDrugs),
	"no_smoking":           neg("smoking"),
	"yes_marijuana":        pos("marijuana", moderation.CategoryDrugs),
	"no_marijuana":         neg("marijuana"),
	"yes_drinking_alcohol": pos("alcohol", moderation.CategoryDrugs),
	"yes_alcohol":          pos("alcohol", moderation.CategoryDrugs),
	"animated_alcohol":     pos("alcohol", moderation.CategoryDrugs),
	"no_alcohol":           neg("alcohol"),
	// Gambling is a vice but not a drug; no dedicated canonical category -> OTHER.
	"yes_gambling": pos("gambling", moderation.CategoryOther),
	"no_gambling":  neg("gambling"),

	// --- Hate / Bullying heads ------------------------------------------------
	"yes_nazi":          pos("nazi", moderation.CategoryHate),
	"no_nazi":           neg("nazi"),
	"yes_terrorist":     pos("terrorist", moderation.CategoryHate),
	"no_terrorist":      neg("terrorist"),
	"yes_kkk":           pos("kkk", moderation.CategoryHate),
	"no_kkk":            neg("kkk"),
	"yes_confederate":   pos("confederate", moderation.CategoryHate),
	"no_confederate":    neg("confederate"),
	"yes_middle_finger": pos("middle_finger", moderation.CategoryHate),
	"no_middle_finger":  neg("middle_finger"),

	// --- Descriptive / non-harm heads (deliberately not emitted) --------------
	"text":               skip("text"),
	"no_text":            neg("text"),
	"yes_overlay_text":   skip("overlay_text"),
	"no_overlay_text":    neg("overlay_text"),
	"yes_qr_code":        skip("qr_code"),
	"no_qr_code":         neg("qr_code"),
	"yes_child_present":  skip("child_present"),
	"no_child_present":   neg("child_present"),
	"yes_religious_icon": skip("religious_icon"),
	"no_religious_icon":  neg("religious_icon"),
	"yes_drawing":        skip("drawing"),
	"no_drawing":         neg("drawing"),
	// Image-type head is mutually-exclusive descriptive classes with no negative.
	"animated": skip("image_type"),
	"hybrid":   skip("image_type"),
	"natural":  skip("image_type"),
}

// adapterCategories is the set of canonical categories this adapter can emit,
// surfaced via Caps.Categories. Derived once from the taxonomy so it never drifts
// from the table. catSkip and the negative placeholder are excluded; OTHER is
// always reachable via the unknown-class fallback.
func adapterCategories() []moderation.Category {
	seen := map[moderation.Category]bool{moderation.CategoryOther: true}
	for _, info := range classTaxonomy {
		if info.pol == positive && info.cat != catSkip {
			seen[info.cat] = true
		}
	}
	out := make([]moderation.Category, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sortCategories(out)
	return out
}

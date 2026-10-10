package codex

const (
	playbookNoteHeading   = "## playbook メモ追記"
	playbookNoteTargetKey = "対象"
	playbookNoteBodyKey   = "内容"
)

// PlaybookNote is what a turn learned that a playbook does not say yet, or
// says differently now. Target is the playbook name as the model wrote it;
// the bot decides whether it names a playbook in the catalog.
type PlaybookNote struct {
	Target string
	Body   string
}

// SplitPlaybookNote removes the section that adds a note to a playbook. The
// section is its heading followed by the bullets "- 対象: ..." and
// "- 内容: ...", each once, and ends at the first other line. It returns the
// note, or nil when there is no section. invalid reports a section that
// appears more than once or has incomplete bullets; it yields no note.
func SplitPlaybookNote(text string) (rest string, note *PlaybookNote, invalid bool) {
	rest, fields, present, invalid := splitFieldSection(text, playbookNoteHeading, playbookNoteTargetKey, playbookNoteBodyKey)
	if !present {
		return text, nil, false
	}
	if invalid || fields[playbookNoteTargetKey] == "" || fields[playbookNoteBodyKey] == "" {
		return rest, nil, true
	}
	return rest, &PlaybookNote{Target: fields[playbookNoteTargetKey], Body: fields[playbookNoteBodyKey]}, false
}

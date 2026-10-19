package richtext

import "reflect"

// Normalize puts a node in canonical form: marks in nesting order, adjacent runs of text with the same marks merged, empty text dropped, empty slices
// nil. Two documents that mean the same thing are equal after it.
func Normalize(n Node) Node {
	n.Marks = sortedMarks(n.Marks)
	if len(n.Marks) == 0 {
		n.Marks = nil
	}
	var out []Node
	for _, c := range n.Content {
		c = Normalize(c)
		if c.Type == Text && c.Text == "" {
			continue
		}
		if c.Type == Text && len(out) > 0 {
			last := &out[len(out)-1]
			if last.Type == Text && reflect.DeepEqual(last.Marks, c.Marks) {
				last.Text += c.Text
				continue
			}
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		out = nil
	}
	n.Content = out
	return n
}

// Equal reports whether two documents are the same after normalizing.
func Equal(a, b Node) bool { return reflect.DeepEqual(Normalize(a), Normalize(b)) }

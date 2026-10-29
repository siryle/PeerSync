package syncer

// Note is one finished sync step, reported to the client log.
type Note struct {
	Op     string
	Path   string
	Detail string
}

// Describe turns a sync step into the line shown in the client.
func Describe(n Note) (level, text string) {
	switch n.Op {
	case opPull:
		return "info", "收到 " + n.Path
	case opDelete:
		return "info", "删除 " + n.Path
	case opDir:
		return "info", "新建目录 " + n.Path
	case opConflict:
		return "warn", "冲突，两边的修改都保留 " + n.Path
	case "send":
		return "info", "发送 " + n.Path
	case "fail":
		text = "失败 " + n.Path
		if n.Detail != "" {
			text += "：" + n.Detail
		}
		return "warn", text
	default:
		if n.Path != "" {
			return "info", n.Path
		}
		return "info", n.Detail
	}
}

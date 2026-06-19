// Package tui renders the interactive inventory TUI (the default `shellkit`
// view) and the non-interactive list/check CLI output (table + JSON). Deps:
// inventory, sshconn, ui.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caoer/shellkit/internal/inventory"
	"github.com/caoer/shellkit/internal/sshconn"
	"github.com/caoer/shellkit/internal/ui"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	selStyle      = lipgloss.NewStyle().Background(lipgloss.Color("24")).Bold(true)
	selGutterChar = lipgloss.NewStyle().Foreground(lipgloss.Color("117")).Bold(true).Render("▎")
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	pendStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	statusBar     = lipgloss.NewStyle().Background(lipgloss.Color("235")).Foreground(lipgloss.Color("252"))
)

type serverRow struct {
	server *inventory.Server
	result *sshconn.ProbeResult
}

type model struct {
	servers  []inventory.Server
	rows     []serverRow
	filtered []int
	cursor   int
	offset   int
	height   int
	width    int

	results    []sshconn.ProbeResult
	probing    bool
	probeDone  int
	probeTotal int

	filter    textinput.Model
	filtering bool

	sortCol  int
	sortAsc  bool
	flashMsg string
	selected map[int]bool
}

type probeResultMsg struct {
	result sshconn.ProbeResult
	done   int
	total  int
}

type probeDoneMsg struct{}

func initialModel(servers []inventory.Server) model {
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Group != servers[j].Group {
			return servers[i].Group < servers[j].Group
		}
		if servers[i].Provider != servers[j].Provider {
			return servers[i].Provider < servers[j].Provider
		}
		return servers[i].Name < servers[j].Name
	})

	rows := make([]serverRow, len(servers))
	results := make([]sshconn.ProbeResult, len(servers))
	for i := range servers {
		rows[i] = serverRow{server: &servers[i]}
		results[i] = sshconn.ProbeResult{Server: &servers[i], Status: sshconn.StatusPending}
		rows[i].result = &results[i]
	}

	filtered := make([]int, len(rows))
	for i := range filtered {
		filtered[i] = i
	}

	fi := textinput.New()
	fi.Prompt = "/"
	fi.CharLimit = 64

	return model{
		servers:  servers,
		rows:     rows,
		filtered: filtered,
		results:  results,
		height:   24,
		width:    120,
		filter:   fi,
		sortAsc:  true,
		selected: make(map[int]bool),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(tea.EnterAltScreen, readProbeChannel())
}

func readProbeChannel() tea.Cmd {
	return func() tea.Msg {
		if probeCh == nil {
			return probeDoneMsg{}
		}
		msg, ok := <-probeCh
		if !ok {
			return probeDoneMsg{}
		}
		return msg
	}
}

func (m *model) sortFiltered() {
	sort.SliceStable(m.filtered, func(a, b int) bool {
		ia, ib := m.filtered[a], m.filtered[b]
		ra, rb := m.rows[ia], m.rows[ib]
		var cmp int
		switch m.sortCol {
		case 0:
			cmp = strings.Compare(ra.server.Group, rb.server.Group)
		case 1:
			cmp = strings.Compare(ra.server.Provider, rb.server.Provider)
		case 2:
			cmp = strings.Compare(ra.server.Name, rb.server.Name)
		case 3:
			cmp = strings.Compare(ra.server.IP, rb.server.IP)
		case 4:
			pa, pb := ra.server.Port, rb.server.Port
			if pa == 0 {
				pa = 22
			}
			if pb == 0 {
				pb = 22
			}
			cmp = pa - pb
		case 5:
			cmp = strings.Compare(ra.server.DisplayUser(), rb.server.DisplayUser())
		case 6:
			cmp = int(ra.result.Status) - int(rb.result.Status)
		case 7:
			la, lb := ra.result.Latency, rb.result.Latency
			if la < lb {
				cmp = -1
			} else if la > lb {
				cmp = 1
			}
		}
		if !m.sortAsc {
			cmp = -cmp
		}
		if cmp != 0 {
			return cmp < 0
		}
		// stable secondary sort: provider then name
		if ra.server.Provider != rb.server.Provider {
			return ra.server.Provider < rb.server.Provider
		}
		return ra.server.Name < rb.server.Name
	})
}

func (m *model) applyFilter() {
	query := strings.ToLower(m.filter.Value())
	m.filtered = m.filtered[:0]
	for i, row := range m.rows {
		if query == "" {
			m.filtered = append(m.filtered, i)
			continue
		}
		searchable := strings.ToLower(fmt.Sprintf("%s %s %s %s %s %s %s %s %s",
			row.server.Group, row.server.Provider, row.server.Name, row.server.IP,
			row.server.SSHAlias, row.server.Project, row.server.Location,
			row.server.Orb, row.result.Status.String()))
		if strings.Contains(searchable, query) {
			m.filtered = append(m.filtered, i)
		}
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = max(0, len(m.filtered)-1)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case probeResultMsg:
		for i := range m.results {
			if m.results[i].Server == msg.result.Server {
				m.results[i] = msg.result
				break
			}
		}
		m.probeDone = msg.done
		m.probeTotal = msg.total
		return m, readProbeChannel()

	case probeDoneMsg:
		m.probing = false
		return m, nil

	case tea.KeyMsg:
		if m.filtering {
			switch msg.String() {
			case "enter", "esc":
				m.filtering = false
				m.filter.Blur()
				return m, nil
			default:
				var cmd tea.Cmd
				m.filter, cmd = m.filter.Update(msg)
				m.applyFilter()
				return m, cmd
			}
		}

		_ = m.flashMsg
		m.flashMsg = ""

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			}
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(m.filtered) - 1
		case "ctrl+u":
			m.cursor -= m.tableHeight() / 2
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "ctrl+d":
			m.cursor += m.tableHeight() / 2
			if m.cursor >= len(m.filtered) {
				m.cursor = max(0, len(m.filtered)-1)
			}
		case "pgup":
			m.cursor -= m.tableHeight()
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "pgdown":
			m.cursor += m.tableHeight()
			if m.cursor >= len(m.filtered) {
				m.cursor = max(0, len(m.filtered)-1)
			}
		case "/":
			m.filtering = true
			m.filter.SetValue("")
			m.filter.Focus()
			m.applyFilter()
			return m, textinput.Blink
		case "esc":
			if len(m.selected) > 0 {
				m.selected = make(map[int]bool)
			} else {
				m.filter.SetValue("")
				m.applyFilter()
			}
		case " ":
			if len(m.filtered) > 0 {
				idx := m.filtered[m.cursor]
				if m.selected[idx] {
					delete(m.selected, idx)
				} else {
					m.selected[idx] = true
				}
				if m.cursor < len(m.filtered)-1 {
					m.cursor++
				}
			}
		case "V":
			if len(m.selected) > 0 {
				m.selected = make(map[int]bool)
			} else {
				for _, idx := range m.filtered {
					m.selected[idx] = true
				}
			}
		case "1", "2", "3", "4", "5", "6", "7", "8":
			col := int(msg.String()[0] - '1')
			if m.sortCol == col {
				m.sortAsc = !m.sortAsc
			} else {
				m.sortCol = col
				m.sortAsc = true
			}
			m.sortFiltered()
		case "c":
			var indices []int
			if len(m.selected) > 0 {
				for _, idx := range m.filtered {
					if m.selected[idx] {
						indices = append(indices, idx)
					}
				}
			} else if len(m.filtered) > 0 {
				indices = []int{m.filtered[m.cursor]}
			}
			if len(indices) > 0 {
				for _, idx := range indices {
					m.results[idx].Status = sshconn.StatusPending
					m.results[idx].Latency = 0
					m.results[idx].KeyUsed = ""
					m.results[idx].Error = ""
				}
				ch := make(chan probeResultMsg, len(indices))
				probeCh = ch
				go func() {
					var wg sync.WaitGroup
					sem := make(chan struct{}, 20)
					done := 0
					var mu sync.Mutex
					for _, idx := range indices {
						wg.Add(1)
						go func(i int) {
							defer wg.Done()
							sem <- struct{}{}
							defer func() { <-sem }()
							r := sshconn.ProbeServer(&m.servers[i], 5*time.Second)
							mu.Lock()
							done++
							d := done
							mu.Unlock()
							ch <- probeResultMsg{result: r, done: d, total: len(indices)}
						}(idx)
					}
					wg.Wait()
					close(ch)
				}()
				return m, readProbeChannel()
			}
		case "C":
			m.probing = true
			m.probeDone = 0
			for i := range m.results {
				m.results[i].Status = sshconn.StatusPending
				m.results[i].Latency = 0
				m.results[i].KeyUsed = ""
				m.results[i].Error = ""
			}
			ch := make(chan probeResultMsg, len(m.servers))
			probeCh = ch
			servers := m.servers
			go func() {
				sshconn.ProbeAll(servers, 20, 5*time.Second, func(r sshconn.ProbeResult, done, total int) {
					ch <- probeResultMsg{result: r, done: done, total: total}
				})
				close(ch)
			}()
			return m, readProbeChannel()
		case "y":
			items := m.selectedOrCurrent()
			if len(items) > 0 {
				var lines []string
				for _, s := range items {
					lines = append(lines, sshconn.SSHCommandString(s))
				}
				text := strings.Join(lines, "\n")
				if copyToClipboard(text) == nil {
					if len(items) == 1 {
						m.flashMsg = "copied: " + lines[0]
					} else {
						m.flashMsg = fmt.Sprintf("copied %d ssh commands", len(items))
					}
				}
			}
		case "Y":
			items := m.selectedOrCurrent()
			if len(items) > 0 {
				var lines []string
				for _, s := range items {
					if s.IP != "" {
						lines = append(lines, s.IP)
					}
				}
				if len(lines) > 0 {
					text := strings.Join(lines, "\n")
					if copyToClipboard(text) == nil {
						if len(lines) == 1 {
							m.flashMsg = "copied: " + lines[0]
						} else {
							m.flashMsg = fmt.Sprintf("copied %d IPs", len(lines))
						}
					}
				}
			}
		case "enter":
			if len(m.filtered) > 0 {
				idx := m.filtered[m.cursor]
				s := m.rows[idx].server
				return m, m.sshInto(s)
			}
		}
	}
	return m, nil
}

var probeCh chan probeResultMsg

func (m model) selectedOrCurrent() []*inventory.Server {
	if len(m.selected) > 0 {
		var out []*inventory.Server
		for _, idx := range m.filtered {
			if m.selected[idx] {
				out = append(out, m.rows[idx].server)
			}
		}
		return out
	}
	if len(m.filtered) > 0 {
		idx := m.filtered[m.cursor]
		return []*inventory.Server{m.rows[idx].server}
	}
	return nil
}

func (m model) sshInto(s *inventory.Server) tea.Cmd {
	return tea.ExecProcess(sshconn.SSHCommand(s), func(err error) tea.Msg {
		return nil
	})
}

func copyToClipboard(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}

// shortPath abbreviates a key path under the home directory to "~/…" for
// compact display in the host table.
func shortPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if len(p) > len(home) && p[:len(home)] == home {
		return "~" + p[len(home):]
	}
	return p
}

func (m model) tableHeight() int {
	return m.height - 5
}

func (m model) View() string {
	var b strings.Builder

	title := headerStyle.Render("shellkit")
	stats := m.statsLine()
	if m.flashMsg != "" {
		fmt.Fprintf(&b, " %s  %s  %s\n", title, stats, okStyle.Render(m.flashMsg))
	} else {
		fmt.Fprintf(&b, " %s  %s\n", title, stats)
	}

	if m.filtering {
		b.WriteString(" " + m.filter.View() + "\n")
	} else if m.filter.Value() != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf(" /%s", m.filter.Value())) + "\n")
	} else {
		b.WriteString("\n")
	}

	cols := []struct {
		header string
		width  int
	}{
		{"GROUP", 10},
		{"PROVIDER", 12},
		{"NAME", 20},
		{"IP", 17},
		{"PORT", 6},
		{"USER", 10},
		{"STATUS", 10},
		{"RTT", 9},
		{"KEY", 0},
	}

	remaining := m.width - 2
	for _, c := range cols[:len(cols)-1] {
		remaining -= c.width
	}
	if remaining > 10 {
		cols[len(cols)-1].width = remaining
	} else {
		cols[len(cols)-1].width = 10
	}

	header := " "
	for i, c := range cols {
		label := c.header
		if i == m.sortCol {
			if m.sortAsc {
				label += " ^"
			} else {
				label += " v"
			}
		}
		header += fmt.Sprintf("%-*s", c.width, label)
	}
	b.WriteString(dimStyle.Render(header) + "\n")

	th := m.tableHeight()
	margin := th / 4
	if margin < 2 {
		margin = 2
	}
	if m.cursor < m.offset+margin {
		m.offset = m.cursor - margin
	}
	if m.cursor >= m.offset+th-margin {
		m.offset = m.cursor - th + margin + 1
	}
	if m.offset < 0 {
		m.offset = 0
	}
	if mx := len(m.filtered) - th; m.offset > mx && mx > 0 {
		m.offset = mx
	}

	end := m.offset + th
	if end > len(m.filtered) {
		end = len(m.filtered)
	}

	for vi := m.offset; vi < end; vi++ {
		idx := m.filtered[vi]
		row := m.rows[idx]
		s := row.server
		r := row.result

		port := s.Port
		if port == 0 {
			port = 22
		}

		statusStr := r.Status.Symbol()
		paddedStatus := fmt.Sprintf("%-*s", cols[6].width, statusStr)
		var styledStatus string
		switch r.Status {
		case sshconn.StatusAuthOK:
			styledStatus = okStyle.Render(paddedStatus)
		case sshconn.StatusSSHOK:
			styledStatus = warnStyle.Render(paddedStatus)
		case sshconn.StatusUnreachable:
			styledStatus = errStyle.Render(paddedStatus)
		case sshconn.StatusAuthFail:
			styledStatus = warnStyle.Render(paddedStatus)
		default:
			styledStatus = pendStyle.Render(paddedStatus)
		}

		keyStr := ""
		if r.KeyUsed != "" {
			keyStr = shortPath(r.KeyUsed)
		}

		group := s.Group
		if group == "" {
			group = "-"
		}

		prefix := " "
		if m.selected[idx] {
			prefix = "*"
		}

		line := fmt.Sprintf("%s%-*s%-*s%-*s%-*s%-*d%-*s%s%-*s%s",
			prefix,
			cols[0].width, ui.Truncate(group, cols[0].width-1),
			cols[1].width, ui.Truncate(s.Provider, cols[1].width-1),
			cols[2].width, ui.Truncate(s.Name, cols[2].width-1),
			cols[3].width, ui.Truncate(s.IP, cols[3].width-1),
			cols[4].width, port,
			cols[5].width, ui.Truncate(s.DisplayUser(), cols[5].width-1),
			styledStatus,
			cols[7].width, sshconn.FormatLatency(r.Latency),
			ui.Truncate(keyStr, cols[8].width),
		)

		switch {
		case vi == m.cursor:
			pad := m.width - ui.VisibleLen(line) - 1
			if pad < 0 {
				pad = 0
			}
			b.WriteString(selGutterChar + selStyle.Render(line+strings.Repeat(" ", pad)))
		case m.selected[idx]:
			b.WriteString(okStyle.Render(line))
		default:
			b.WriteString(line)
		}
		b.WriteString("\n")
	}

	for i := end - m.offset; i < th; i++ {
		b.WriteString("\n")
	}

	selInfo := ""
	if n := len(m.selected); n > 0 {
		selInfo = fmt.Sprintf("  sel:%d", n)
	}
	help := statusBar.Render(fmt.Sprintf(" [spc]sel  [V]all  [y]cmd  [Y]ip  [c]heck  [C]all  [/]filter  [1-8]sort  [enter]ssh  [q]uit  %d/%d%s ",
		len(m.filtered), len(m.rows), selInfo))
	b.WriteString(help)

	return b.String()
}

func (m model) statsLine() string {
	var online, authOK, down, pending int
	for _, r := range m.results {
		switch r.Status {
		case sshconn.StatusAuthOK:
			online++
			authOK++
		case sshconn.StatusSSHOK:
			online++
		case sshconn.StatusUnreachable:
			down++
		case sshconn.StatusPending:
			pending++
		}
	}

	parts := []string{
		fmt.Sprintf("total:%d", len(m.servers)),
	}
	if pending > 0 {
		parts = append(parts, pendStyle.Render(fmt.Sprintf("checking:%d", pending)))
	}
	if authOK > 0 {
		parts = append(parts, okStyle.Render(fmt.Sprintf("auth-ok:%d", authOK)))
	}
	if online > 0 {
		parts = append(parts, warnStyle.Render(fmt.Sprintf("ssh-ok:%d", online-authOK)))
	}
	if down > 0 {
		parts = append(parts, errStyle.Render(fmt.Sprintf("down:%d", down)))
	}
	return strings.Join(parts, "  ")
}

func RunTUI(servers []inventory.Server) error {
	m := initialModel(servers)
	ch := make(chan probeResultMsg, len(servers))
	probeCh = ch

	go func() {
		sshconn.ProbeAll(servers, 20, 5*time.Second, func(r sshconn.ProbeResult, done, total int) {
			ch <- probeResultMsg{result: r, done: done, total: total}
		})
		close(ch)
	}()

	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

func CLIList(servers []inventory.Server, jsonOutput bool) {
	if jsonOutput {
		printJSON(servers)
		return
	}
	fmt.Printf("%-10s %-14s %-20s %-17s %6s %-10s %-8s %-9s %s\n",
		"GROUP", "PROVIDER", "NAME", "IP", "PORT", "USER", "STATE", "MANAGED", "LOCATION")
	fmt.Println(strings.Repeat("-", 115))
	sort.Slice(servers, func(i, j int) bool {
		if servers[i].Group != servers[j].Group {
			return servers[i].Group < servers[j].Group
		}
		if servers[i].Provider != servers[j].Provider {
			return servers[i].Provider < servers[j].Provider
		}
		return servers[i].Name < servers[j].Name
	})
	for _, s := range servers {
		port := s.Port
		if port == 0 {
			port = 22
		}
		state := s.State
		if state == "" {
			state = "-"
		}
		loc := s.Location
		if loc == "" {
			loc = "-"
		}
		group := s.Group
		if group == "" {
			group = "-"
		}
		managed := s.Managed
		if managed == "" {
			managed = "-"
		}
		fmt.Printf("%-10s %-14s %-20s %-17s %6d %-10s %-8s %-9s %s\n",
			group, s.Provider, s.Name, s.IP, port, s.DisplayUser(), state, managed, loc)
	}
}

func CLICheck(servers []inventory.Server, jsonOutput bool) {
	fmt.Fprintf(os.Stderr, "Probing %d servers...\n", len(servers))
	results := sshconn.ProbeAll(servers, 20, 5*time.Second, func(r sshconn.ProbeResult, done, total int) {
		fmt.Fprintf(os.Stderr, "\r  %d/%d", done, total)
	})
	fmt.Fprintln(os.Stderr)

	if jsonOutput {
		printCheckJSON(results)
		return
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Server.Provider != results[j].Server.Provider {
			return results[i].Server.Provider < results[j].Server.Provider
		}
		return results[i].Server.Name < results[j].Server.Name
	})

	fmt.Printf("%-14s %-20s %-17s %-8s %8s  %s\n",
		"PROVIDER", "NAME", "IP", "STATUS", "LATENCY", "KEY")
	fmt.Println(strings.Repeat("-", 95))
	for _, r := range results {
		key := ""
		if r.KeyUsed != "" {
			key = shortPath(r.KeyUsed)
		}
		fmt.Printf("%-14s %-20s %-17s %-8s %8s  %s\n",
			r.Server.Provider, r.Server.Name, r.Server.IP,
			r.Status.String(), sshconn.FormatLatency(r.Latency), key)
	}

	var online, authOK, down int
	for _, r := range results {
		switch r.Status {
		case sshconn.StatusAuthOK:
			online++
			authOK++
		case sshconn.StatusSSHOK:
			online++
		case sshconn.StatusUnreachable:
			down++
		}
	}
	fmt.Printf("\nTotal: %d  Auth-OK: %d  SSH-OK: %d  Down: %d\n",
		len(results), authOK, online-authOK, down)
}

type listEntryJSON struct {
	Group      string `json:"group"`
	Provider   string `json:"provider"`
	Name       string `json:"name"`
	IP         string `json:"wan_ip"`
	ResolvedIP string `json:"resolved_ip,omitempty"`
	Port       int    `json:"port"`
	SSHPort    int    `json:"ssh_port"`
	User       string `json:"user"`
	Identity   string `json:"identity,omitempty"`
	Location   string `json:"location"`
	State      string `json:"state"`
	Project    string `json:"project"`
	Managed    string `json:"managed,omitempty"`
	SSHAlias   string `json:"ssh_alias,omitempty"`
}

type checkEntryJSON struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	IP        string `json:"wan_ip"`
	Status    string `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Key       string `json:"key"`
}

func writeJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "json encode error: %v\n", err)
	}
}

func printJSON(servers []inventory.Server) {
	out := make([]listEntryJSON, 0, len(servers))
	for _, s := range servers {
		port := s.Port
		if port == 0 {
			port = 22
		}
		resolved := s.ResolvedIP()
		sshPort := s.PortFor(resolved)
		out = append(out, listEntryJSON{
			Group:      s.Group,
			Provider:   s.Provider,
			Name:       s.Name,
			IP:         s.IP,
			ResolvedIP: resolved,
			Port:       port,
			SSHPort:    sshPort,
			User:       s.DisplayUser(),
			Identity:   s.Identity,
			Location:   s.Location,
			State:      s.State,
			Project:    s.Project,
			Managed:    s.Managed,
			SSHAlias:   s.SSHAlias,
		})
	}
	writeJSON(out)
}

func printCheckJSON(results []sshconn.ProbeResult) {
	out := make([]checkEntryJSON, 0, len(results))
	for _, r := range results {
		out = append(out, checkEntryJSON{
			Provider:  r.Server.Provider,
			Name:      r.Server.Name,
			IP:        r.Server.IP,
			Status:    r.Status.String(),
			LatencyMs: r.Latency.Milliseconds(),
			Key:       r.KeyUsed,
		})
	}
	writeJSON(out)
}

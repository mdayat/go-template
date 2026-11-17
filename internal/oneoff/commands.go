package oneoff

// Commands are every command cmd/oneoff runs. Add a new one here, in its own file.
func Commands() []Command {
	return []Command{
		trimTaskTitles(),
	}
}

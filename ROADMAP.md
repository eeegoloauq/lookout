# Roadmap

Larger work that shouldn't be done in passing. Remove an item when it ships.

- **Bound mute requests.** Add a request body size limit to the mute and unmute handlers in `internal/web/mute.go`.
- **Test host updates.** Cover checksum rejection, config validation, health failure, and rollback in `contrib/lookout-update`.
- **Keep the board usable during inspection.** Review the forced reload after a detail row stays open and the `:has()` dependency in `internal/web/page.html` and `internal/web/page.css`.
- **Make large components easier to maintain.** Split focused responsibilities out of `internal/config/load.go`, `internal/web/page.go`, and `internal/monitor/monitor.go` when working on those areas.

//go:build darwin && cgo

#ifndef TRACKKR_MACOS_DARWIN_H
#define TRACKKR_MACOS_DARWIN_H

#include <stdbool.h>
#include <sys/types.h>

typedef struct {
    char *name;
    pid_t pid;
} trackkr_app;

#define TRACKKR_OK 0
#define TRACKKR_NO_APP 1
#define TRACKKR_FAILED 2

int trackkr_frontmost_app(trackkr_app *out);
char *trackkr_focused_window_title(pid_t pid);
bool trackkr_is_accessibility_trusted(void);
void trackkr_prompt_for_accessibility(void);

#endif

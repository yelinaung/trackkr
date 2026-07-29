//go:build darwin && cgo

#import <ApplicationServices/ApplicationServices.h>
#import <CoreGraphics/CoreGraphics.h>
#import <Foundation/Foundation.h>

#include <stdlib.h>
#include <string.h>

#include "macos_darwin.h"

static char *trackkr_copy_string(CFStringRef value) {
    if (value == NULL) {
        return calloc(1, 1);
    }

    CFIndex length = CFStringGetLength(value);
    CFIndex capacity = CFStringGetMaximumSizeForEncoding(
        length, kCFStringEncodingUTF8) + 1;
    char *result = malloc((size_t)capacity);
    if (result == NULL) {
        return NULL;
    }
    if (!CFStringGetCString(value, result, capacity, kCFStringEncodingUTF8)) {
        free(result);
        return NULL;
    }
    return result;
}

int trackkr_frontmost_app(trackkr_app *out) {
    if (out == NULL) {
        return TRACKKR_FAILED;
    }
    memset(out, 0, sizeof(*out));

    @autoreleasepool {
        CFArrayRef windowList = CGWindowListCopyWindowInfo(
            kCGWindowListOptionOnScreenOnly |
                kCGWindowListExcludeDesktopElements,
            kCGNullWindowID);
        if (windowList == NULL) {
            return TRACKKR_FAILED;
        }

        NSArray *windows = (NSArray *)windowList;
        for (NSDictionary *window in windows) {
            NSNumber *layer = [window objectForKey:(id)kCGWindowLayer];
            if (layer == nil || [layer integerValue] != 0) {
                continue;
            }

            NSString *owner = [window objectForKey:(id)kCGWindowOwnerName];
            NSNumber *ownerPID = [window objectForKey:(id)kCGWindowOwnerPID];
            if (owner == nil || ownerPID == nil) {
                continue;
            }

            out->name = trackkr_copy_string((CFStringRef)owner);
            if (out->name == NULL) {
                CFRelease(windowList);
                return TRACKKR_FAILED;
            }
            out->pid = (pid_t)[ownerPID intValue];
            CFRelease(windowList);
            return TRACKKR_OK;
        }

        CFRelease(windowList);
        return TRACKKR_NO_APP;
    }
}

char *trackkr_focused_window_title(pid_t pid) {
    @autoreleasepool {
        AXUIElementRef application = AXUIElementCreateApplication(pid);
        if (application == NULL) {
            return NULL;
        }
        AXError error = AXUIElementSetMessagingTimeout(application, 0.5);
        if (error != kAXErrorSuccess) {
            CFRelease(application);
            return NULL;
        }

        CFTypeRef focusedWindow = NULL;
        error = AXUIElementCopyAttributeValue(
            application, kAXFocusedWindowAttribute, &focusedWindow);
        if (error != kAXErrorSuccess || focusedWindow == NULL) {
            CFRelease(application);
            return NULL;
        }
        if (CFGetTypeID(focusedWindow) != AXUIElementGetTypeID()) {
            CFRelease(focusedWindow);
            CFRelease(application);
            return NULL;
        }

        error = AXUIElementSetMessagingTimeout(
            (AXUIElementRef)focusedWindow, 0.5);
        if (error != kAXErrorSuccess) {
            CFRelease(focusedWindow);
            CFRelease(application);
            return NULL;
        }

        CFTypeRef title = NULL;
        error = AXUIElementCopyAttributeValue(
            (AXUIElementRef)focusedWindow, kAXTitleAttribute, &title);
        char *result = NULL;
        if (error == kAXErrorSuccess && title != NULL &&
            CFGetTypeID(title) == CFStringGetTypeID()) {
            result = trackkr_copy_string((CFStringRef)title);
        }

        if (title != NULL) {
            CFRelease(title);
        }
        CFRelease(focusedWindow);
        CFRelease(application);
        return result;
    }
}

bool trackkr_is_accessibility_trusted(void) {
    @autoreleasepool {
        return AXIsProcessTrusted();
    }
}

void trackkr_prompt_for_accessibility(void) {
    @autoreleasepool {
        const void *keys[] = {kAXTrustedCheckOptionPrompt};
        const void *values[] = {kCFBooleanTrue};
        CFDictionaryRef options = CFDictionaryCreate(
            kCFAllocatorDefault,
            keys,
            values,
            1,
            &kCFTypeDictionaryKeyCallBacks,
            &kCFTypeDictionaryValueCallBacks);
        if (options == NULL) {
            return;
        }
        AXIsProcessTrustedWithOptions(options);
        CFRelease(options);
    }
}

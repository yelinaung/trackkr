//go:build darwin && cgo

#import <ApplicationServices/ApplicationServices.h>
#import <AppKit/AppKit.h>
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

bool trackkr_app_icon_png(pid_t pid, trackkr_png *out) {
    if (out == NULL) {
        return false;
    }
    memset(out, 0, sizeof(*out));

    @autoreleasepool {
        NSRunningApplication *application =
            [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
        NSString *applicationName = [application localizedName];
        NSImage *source = [application icon];
        if (applicationName == nil || source == nil || source.size.width <= 0 ||
            source.size.height <= 0) {
            return false;
        }

        char *name = trackkr_copy_string((CFStringRef)applicationName);
        if (name == NULL) {
            return false;
        }

        const NSInteger edge = 64;
        NSBitmapImageRep *bitmap = [[NSBitmapImageRep alloc]
            initWithBitmapDataPlanes:NULL
            pixelsWide:edge
            pixelsHigh:edge
            bitsPerSample:8
            samplesPerPixel:4
            hasAlpha:YES
            isPlanar:NO
            colorSpaceName:NSCalibratedRGBColorSpace
            bytesPerRow:0
            bitsPerPixel:0];
        if (bitmap == nil) {
            free(name);
            return false;
        }
        [bitmap setSize:NSMakeSize(edge, edge)];
        unsigned char *bitmapBytes = [bitmap bitmapData];
        if (bitmapBytes == NULL) {
            [bitmap release];
            free(name);
            return false;
        }
        memset(bitmapBytes, 0,
               (size_t)[bitmap bytesPerRow] * (size_t)edge);

        NSGraphicsContext *context =
            [NSGraphicsContext graphicsContextWithBitmapImageRep:bitmap];
        if (context == nil) {
            [bitmap release];
            free(name);
            return false;
        }

        CGFloat scale = MIN((CGFloat)edge / source.size.width,
                            (CGFloat)edge / source.size.height);
        NSSize fitted = NSMakeSize(source.size.width * scale,
                                   source.size.height * scale);
        NSRect destination = NSMakeRect(
            ((CGFloat)edge - fitted.width) / 2.0,
            ((CGFloat)edge - fitted.height) / 2.0,
            fitted.width,
            fitted.height);

        [NSGraphicsContext saveGraphicsState];
        [NSGraphicsContext setCurrentContext:context];
        [context setImageInterpolation:NSImageInterpolationHigh];
        [source drawInRect:destination
                  fromRect:NSZeroRect
                 operation:NSCompositingOperationSourceOver
                  fraction:1.0
            respectFlipped:YES
                     hints:nil];
        [NSGraphicsContext restoreGraphicsState];

        NSData *data = [bitmap
            representationUsingType:NSBitmapImageFileTypePNG
            properties:@{}];
        if (data == nil || data.length == 0) {
            [bitmap release];
            free(name);
            return false;
        }

        unsigned char *copy = malloc(data.length);
        if (copy == NULL) {
            [bitmap release];
            free(name);
            return false;
        }
        memcpy(copy, data.bytes, data.length);
        out->name = name;
        out->bytes = copy;
        out->length = data.length;
        [bitmap release];
        return true;
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

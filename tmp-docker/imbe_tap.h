#pragma once
// imbe_tap.h — File-based tap of IMBE frame parameters after FEC decode.
// Writes binary .tap sidecar files beside audio .wav files.
//
// Drop into: lib/op25_repeater/lib/

#include <stdint.h>
#include <stddef.h>
#include <stdio.h>
#include <string>

#define IMBE_TAP_MAGIC   0x494D4245u  // "IMBE"
#define IMBE_TAP_VERSION 1

// File header — written once at start of each .tap file (8 bytes)
#pragma pack(push, 1)
struct imbe_tap_header {
    uint32_t magic;       // IMBE_TAP_MAGIC
    uint8_t  version;     // IMBE_TAP_VERSION
    uint8_t  reserved[3]; // padding
};

// One record per IMBE voice frame (20ms / 50fps, 32 bytes per frame)
struct imbe_tap_frame {
    uint32_t seq;       // frame sequence number (per-call)
    uint16_t tgid;      // talkgroup ID
    uint32_t src_id;    // source radio ID
    uint16_t errs;      // FEC error count
    uint16_t E0;        // Golay error count for u0
    uint16_t ET;        // cumulative FEC error count
    uint16_t u[8];      // IMBE codeword parameters u[0]..u[7]
};
#pragma pack(pop)

// Tap writer — one instance per p25p1_fdma / per recorder.
class imbe_tap {
public:
    imbe_tap();
    ~imbe_tap();

    // Set output path and open file. Closes any previously open file.
    void open_file(const std::string& path);

    // Close current file.
    void close_file();

    // Append one IMBE frame to the open file. No-op if no file is open.
    void send(uint16_t tgid, uint32_t src_id, size_t errs, uint32_t E0, uint32_t ET, const uint32_t u[8]);

private:
    FILE*       fp_;
    uint32_t    seq_;
    std::string path_;
};

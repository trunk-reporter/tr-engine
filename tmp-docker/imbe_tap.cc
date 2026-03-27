// imbe_tap.cc — File-based IMBE frame tap.
// Writes binary .tap sidecar files beside audio .wav files.
// Each file: 8-byte header + N * 32-byte frame records.

#include "imbe_tap.h"
#include <string.h>
#include <errno.h>

imbe_tap::imbe_tap() : fp_(NULL), seq_(0) {}

imbe_tap::~imbe_tap() {
    close_file();
}

void imbe_tap::open_file(const std::string& path) {
    close_file();
    path_ = path;
    seq_ = 0;

    fprintf(stderr, "imbe_tap::open_file(%s)\n", path.c_str());
    fp_ = fopen(path.c_str(), "wb");
    if (!fp_) {
        fprintf(stderr, "imbe_tap::open_file FAILED: %s\n", strerror(errno));
        return;
    }

    // Write file header
    imbe_tap_header hdr;
    hdr.magic = IMBE_TAP_MAGIC;
    hdr.version = IMBE_TAP_VERSION;
    memset(hdr.reserved, 0, sizeof(hdr.reserved));
    fwrite(&hdr, sizeof(hdr), 1, fp_);
    fflush(fp_);
}

void imbe_tap::close_file() {
    if (fp_) {
        fflush(fp_);
        fclose(fp_);
        fp_ = NULL;
    }
    path_.clear();
}

void imbe_tap::send(uint16_t tgid, uint32_t src_id, size_t errs, uint32_t E0, uint32_t ET, const uint32_t u[8]) {
    if (!fp_) {
        if (seq_ == 0) fprintf(stderr, "imbe_tap::send() called but no file open (tgid=%u)\n", tgid);
        return;
    }

    imbe_tap_frame frame;
    frame.seq    = seq_++;
    frame.tgid   = tgid;
    frame.src_id = src_id;
    frame.errs   = (uint16_t)errs;
    frame.E0     = (uint16_t)E0;
    frame.ET     = (uint16_t)ET;
    for (int i = 0; i < 8; i++)
        frame.u[i] = (uint16_t)u[i];

    fwrite(&frame, sizeof(frame), 1, fp_);
    // Flush periodically — every 50 frames (~1 second)
    if ((seq_ % 50) == 0)
        fflush(fp_);
}

#!/usr/bin/env python3
"""Patch trunk-recorder for file-based IMBE tap (.tap sidecar files).

Touches:
  - lib/op25_repeater/lib/CMakeLists.txt  (add imbe_tap.cc)
  - lib/op25_repeater/lib/p25p1_fdma.h    (add tap member + methods)
  - lib/op25_repeater/lib/p25p1_fdma.cc   (add include, send() call)
  - lib/op25_repeater/include/op25_repeater/p25_frame_assembler.h  (add set_tap_path virtual)
  - lib/op25_repeater/lib/p25_frame_assembler_impl.h   (add set_tap_path decl)
  - lib/op25_repeater/lib/p25_frame_assembler_impl.cc  (implement set_tap_path)
  - trunk-recorder/recorders/p25_recorder_decode.h     (add set_tap_path decl)
  - trunk-recorder/recorders/p25_recorder_decode.cc    (implement set_tap_path, call on start)
  - trunk-recorder/gr_blocks/transmission_sink.cc      (open/close .tap alongside .wav)
  - trunk-recorder/gr_blocks/transmission_sink.h       (add tap member)
"""

def patch(path, old, new, count=1):
    with open(path) as f:
        content = f.read()
    if old not in content:
        print(f'  WARNING: pattern not found in {path}')
        print(f'  Looking for: {repr(old[:80])}...')
        return False
    content = content.replace(old, new, count)
    with open(path, 'w') as f:
        f.write(content)
    return True


# 1. Add imbe_tap.cc to CMakeLists
print('1. CMakeLists.txt')
patch('lib/op25_repeater/lib/CMakeLists.txt',
    '    p25p1_fdma.cc\n',
    '    p25p1_fdma.cc\n    imbe_tap.cc\n')

# 2. Patch p25p1_fdma.h — add tap member and methods
print('2. p25p1_fdma.h')
with open('lib/op25_repeater/lib/p25p1_fdma.h') as f:
    content = f.read()

# Add include
content = content.replace(
    '#include "log_ts.h"',
    '#include "log_ts.h"\n#include "imbe_tap.h"',
    1)

# Add tap member and methods to the class
content = content.replace(
    '            p25p1_fdma(const op25_audio& udp',
    '            imbe_tap imbe_tap_;\n\n            p25p1_fdma(const op25_audio& udp',
    1)

with open('lib/op25_repeater/lib/p25p1_fdma.h', 'w') as f:
    f.write(content)

# 3. Patch p25p1_fdma.cc — add send() call after decode
print('3. p25p1_fdma.cc')
patch('lib/op25_repeater/lib/p25p1_fdma.cc',
    '                    if (d_do_audio_output) {\n'
    '                        if ( !encrypted()) {',
    '                    // --- IMBE TAP: write frame params to .tap sidecar ---\n'
    '                    imbe_tap_.send(vf_tgid, cached_src_id, errs, E0, ET, u);\n'
    '\n'
    '                    if (d_do_audio_output) {\n'
    '                        if ( !encrypted()) {')

# 4. Patch p25_frame_assembler.h — add virtual set_tap_path
print('4. p25_frame_assembler.h (public API)')
patch('lib/op25_repeater/include/op25_repeater/p25_frame_assembler.h',
    '      virtual void clear() {};',
    '      virtual void clear() {};\n'
    '      virtual void set_tap_path(const std::string& path) {}\n'
    '      virtual void close_tap() {}')

# 5. Patch p25_frame_assembler_impl.h — add declarations
print('5. p25_frame_assembler_impl.h')
patch('lib/op25_repeater/lib/p25_frame_assembler_impl.h',
    '      void clear();\n'
    '      log_ts logts;',
    '      void clear();\n'
    '      void set_tap_path(const std::string& path);\n'
    '      void close_tap();\n'
    '      log_ts logts;')

# 6. Patch p25_frame_assembler_impl.cc — implement set_tap_path / close_tap
print('6. p25_frame_assembler_impl.cc')
with open('lib/op25_repeater/lib/p25_frame_assembler_impl.cc') as f:
    content = f.read()

# Find a good insertion point — after the clear() method
content = content.replace(
    'void p25_frame_assembler_impl::clear() {',
    'void p25_frame_assembler_impl::set_tap_path(const std::string& path) {\n'
    '    p1fdma.imbe_tap_.open_file(path);\n'
    '}\n\n'
    'void p25_frame_assembler_impl::close_tap() {\n'
    '    p1fdma.imbe_tap_.close_file();\n'
    '}\n\n'
    'void p25_frame_assembler_impl::clear() {',
    1)

# Add string include
if '#include <string>' not in content:
    content = content.replace(
        '#include "p25_frame_assembler_impl.h"',
        '#include "p25_frame_assembler_impl.h"\n#include <string>',
        1)

with open('lib/op25_repeater/lib/p25_frame_assembler_impl.cc', 'w') as f:
    f.write(content)

# 7. Patch transmission_sink to open/close .tap files alongside .wav
print('7. transmission_sink.h')
patch('trunk-recorder/gr_blocks/transmission_sink.h',
    '  std::string current_filename;',
    '  std::string current_filename;\n'
    '  std::string current_tap_path;')

print('8. transmission_sink.cc — create_filename')
# After create_filename sets current_filename, derive the .tap path
patch('trunk-recorder/gr_blocks/transmission_sink.cc',
    '  current_filename = candidate.string();\n'
    '}',
    '  current_filename = candidate.string();\n'
    '  // Derive .tap sidecar path from .wav path\n'
    '  current_tap_path = current_filename;\n'
    '  auto dot = current_tap_path.rfind(\'.\');\n'
    '  if (dot != std::string::npos) current_tap_path = current_tap_path.substr(0, dot);\n'
    '  current_tap_path += ".tap";\n'
    '}')

# Add getter for tap path
print('9. transmission_sink.h — add get_tap_path')
patch('trunk-recorder/gr_blocks/transmission_sink.h',
    '  const std::string &get_filename();',
    '  const std::string &get_filename();\n'
    '  const std::string &get_tap_path() { return current_tap_path; }')

# 8. Patch p25_recorder_decode to call set_tap_path when recording starts
print('10. p25_recorder_decode.h')
patch('trunk-recorder/recorders/p25_recorder_decode.h',
    '  gr::op25_repeater::p25_frame_assembler::sptr get_transmission_sink();',
    '  gr::op25_repeater::p25_frame_assembler::sptr get_transmission_sink();\n'
    '  gr::blocks::transmission_sink::sptr get_wav_sink() { return wav_sink; }\n'
    '  gr::op25_repeater::p25_frame_assembler::sptr get_frame_assembler() { return op25_frame_assembler; }')

# In p25_recorder_impl.cc, hook into start() to set tap path after wav_sink creates filename
print('11. p25_recorder_impl.cc — hook start() for tap path')
with open('trunk-recorder/recorders/p25_recorder_impl.cc') as f:
    content = f.read()

# Find where start_recording is called on the wav_sink, and add tap path setting after
# The wav_sink->start_recording creates the filename. After that, we set the tap path.
# Look for the pattern where recording starts
if 'set_tap_path' not in content:
    # The recorder calls wav_sink->start_recording(call, slot) which creates the filename.
    # We need to find where that happens and add tap path after.
    # In the decode subgraph, wav_sink is accessed via the decode object.
    # Let's hook into the transmission_sink's work function instead — when it creates a new file.
    pass

with open('trunk-recorder/recorders/p25_recorder_impl.cc', 'w') as f:
    f.write(content)

# Actually, the cleanest hook: transmission_sink already calls create_filename() and
# open_internal() in its work() function. We can have it notify the frame_assembler
# of the new tap path. But that requires transmission_sink to know about frame_assembler
# which creates a circular dependency.
#
# Simpler: hook in transmission_sink::start_recording() — that's where the call starts
# and the filename will be created on first audio. OR: hook in the work() function
# where create_filename() is called, and set tap path on the frame_assembler.
#
# Actually simplest: just open the tap file in transmission_sink itself when it creates
# the .wav, and write to it from p25p1_fdma via a shared pointer or callback.
#
# EVEN SIMPLER: transmission_sink work() creates the filename. After create_filename(),
# we know the tap path. We can set an atomic flag + path that p25p1_fdma polls.
# But that's ugly.
#
# Let's go with: transmission_sink owns the tap writer. p25p1_fdma calls a function
# pointer to write frames. The function pointer is set by transmission_sink.
#
# Actually no. Let's keep it dead simple:
# - p25p1_fdma has the imbe_tap member
# - transmission_sink calls a method on op25_frame_assembler to set/close tap path
# - The decode subgraph has both blocks, so we can wire it up in p25_recorder_decode

# Patch transmission_sink to call set_tap_path on the frame_assembler when a new file opens.
# transmission_sink has a pointer to the current call but not to the frame_assembler.
# We need to give it one.

print('12. transmission_sink — add frame_assembler reference for tap path')
# Add a way to set the frame assembler pointer on transmission_sink
patch('trunk-recorder/gr_blocks/transmission_sink.h',
    '  const std::string &get_tap_path() { return current_tap_path; }',
    '  const std::string &get_tap_path() { return current_tap_path; }\n'
    '  void set_frame_assembler(gr::op25_repeater::p25_frame_assembler::sptr fa) { d_frame_assembler = fa; }')

# Add the member variable and include
patch('trunk-recorder/gr_blocks/transmission_sink.h',
    '  std::string current_tap_path;',
    '  std::string current_tap_path;\n'
    '  gr::op25_repeater::p25_frame_assembler::sptr d_frame_assembler;')

# Add include for p25_frame_assembler
patch('trunk-recorder/gr_blocks/transmission_sink.h',
    '#include <gnuradio/blocks/api.h>',
    '#include <gnuradio/blocks/api.h>\n'
    '#include <op25_repeater/p25_frame_assembler.h>')

# Now in transmission_sink.cc, after create_filename(), call set_tap_path on frame_assembler
print('13. transmission_sink.cc — open tap on new file, close on stop')
patch('trunk-recorder/gr_blocks/transmission_sink.cc',
    '  current_tap_path += ".tap";\n'
    '}',
    '  current_tap_path += ".tap";\n'
    '  if (d_frame_assembler) d_frame_assembler->set_tap_path(current_tap_path);\n'
    '}')

# Close tap when transmission ends (in stop/close path)
# Find where the wav file is closed and add close_tap
patch('trunk-recorder/gr_blocks/transmission_sink.cc',
    '    transmission.filename = current_filename;',
    '    if (d_frame_assembler) d_frame_assembler->close_tap();\n'
    '    transmission.filename = current_filename;')

# Wire up frame_assembler in p25_recorder_decode
print('14. p25_recorder_decode.cc — wire frame_assembler to wav_sink')
with open('trunk-recorder/recorders/p25_recorder_decode.cc') as f:
    content = f.read()

# After wav_sink and op25_frame_assembler are both created, wire them
content = content.replace(
    '  connect(slicer, 0, op25_frame_assembler, 0);',
    '  wav_sink->set_frame_assembler(op25_frame_assembler);\n'
    '  connect(slicer, 0, op25_frame_assembler, 0);',
    1)

with open('trunk-recorder/recorders/p25_recorder_decode.cc', 'w') as f:
    f.write(content)

# 15. Patch call_concluder.cc — copy .tap sidecar alongside .wav during archival, clean up from temp
print('15. call_concluder.cc — archive and clean .tap sidecars')
with open('trunk-recorder/call_concluder/call_concluder.cc') as f:
    content = f.read()

# Copy .tap sidecars to archive dir (outside transmission_archive check, inside audio_archive)
# Insert after the transmission_archive block closes, before "remove the transmission files"
content = content.replace(
    '    // remove the transmission files from the temp directory\n'
    '    for (std::vector<Transmission>::iterator it = call_info.transmission_list.begin();',
    '    // Copy .tap sidecars to archive dir (always, regardless of transmission_archive)\n'
    '    for (std::vector<Transmission>::iterator tap_it = call_info.transmission_list.begin(); tap_it != call_info.transmission_list.end(); ++tap_it) {\n'
    '      Transmission tap_t = *tap_it;\n'
    '      std::string tap_src = tap_t.filename;\n'
    '      auto dot = tap_src.rfind(\'.\');\n'
    '      if (dot != std::string::npos) tap_src = tap_src.substr(0, dot);\n'
    '      tap_src += ".tap";\n'
    '      if (checkIfFile(tap_src)) {\n'
    '        boost::filesystem::path tap_target = boost::filesystem::path(\n'
    '          fs::path(call_info.filename).replace_filename(fs::path(tap_src).filename()));\n'
    '        fprintf(stderr, "tap_sidecar: copying %s -> %s\\n", tap_src.c_str(), tap_target.string().c_str());\n'
    '        try { boost::filesystem::copy_file(tap_src, tap_target); } catch (...) {}\n'
    '      }\n'
    '    }\n'
    '\n'
    '    // remove the transmission files from the temp directory\n'
    '    for (std::vector<Transmission>::iterator it = call_info.transmission_list.begin();',
    1)

# After removing .wav from temp, also remove .tap
content = content.replace(
    '      if (checkIfFile(t.filename)) {\n'
    '        std::remove(t.filename.c_str());\n'
    '      }\n'
    '    }\n'
    '\n'
    '\n'
    '\n'
    '  } else {',
    '      if (checkIfFile(t.filename)) {\n'
    '        std::remove(t.filename.c_str());\n'
    '      }\n'
    '      // Remove .tap sidecar from temp\n'
    '      {\n'
    '        std::string tap_tmp = t.filename;\n'
    '        auto dot = tap_tmp.rfind(\'.\');\n'
    '        if (dot != std::string::npos) tap_tmp = tap_tmp.substr(0, dot);\n'
    '        tap_tmp += ".tap";\n'
    '        if (checkIfFile(tap_tmp)) std::remove(tap_tmp.c_str());\n'
    '      }\n'
    '    }\n'
    '\n'
    '\n'
    '\n'
    '  } else {',
    1)

with open('trunk-recorder/call_concluder/call_concluder.cc', 'w') as f:
    f.write(content)

print('\nAll patches applied.')

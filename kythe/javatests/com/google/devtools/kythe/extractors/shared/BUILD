package(default_visibility = ["//kythe:default_visibility"])

java_test(
    name = "file_vnames_test",
    size = "small",
    srcs = ["FileVNamesTest.java"],
    test_class = "com.google.devtools.kythe.extractors.shared.FileVNamesTest",
    deps = [
        "//kythe/java/com/google/devtools/kythe/extractors/shared:file_vnames",
        "//kythe/proto:storage_proto_java",
        "//third_party/proto:protobuf_java",
        "@com_google_guava_guava//jar",
        "@junit_junit//jar",
    ],
)

java_test(
    name = "extractor_utils_test",
    size = "small",
    srcs = ["ExtractorUtilsTest.java"],
    data = ["testdata/sample.txt"],
    test_class = "com.google.devtools.kythe.extractors.shared.ExtractorUtilsTest",
    deps = [
        "//kythe/java/com/google/devtools/kythe/extractors/shared",
        "//kythe/proto:analysis_proto_java",
        "//kythe/proto:storage_proto_java",
        "//third_party/proto:protobuf_java",
        "@com_google_guava_guava//jar",
        "@junit_junit//jar",
    ],
)

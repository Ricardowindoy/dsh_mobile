# Add project specific ProGuard rules here.
# You can control the set of applied configuration files using the
# proguardFiles setting in build.gradle.
#
# For more details, see
#   http://developer.android.com/guide/developing/tools/proguard.html

# If your project uses WebView with JS, uncomment the following
# and specify the fully qualified class name to the JavaScript interface
# class:
#-keepclassmembers class fqcn.of.javascript.interface.for.webview {
#   public *;
#}

# Uncomment this to preserve the line number information for
# debugging stack traces.
#-keepattributes SourceFile,LineNumberTable

# If you keep the line number information, uncomment this to
# hide the original source file name.
#-renamesourcefileattribute SourceFile

# dsh-mobile: 自动更新 JNI 桥。Rust 侧按 JNI 符号名注册
# Java_com_namelessric_dsh_1mobile_MainActivity_nativeInitUpdater，
# R8 重命名会导致启动即 UnsatisfiedLinkError，必须保住 native 方法名。
-keepclassmembernames class com.namelessric.dsh_mobile.MainActivity {
  native <methods>;
}
-keepclassmembernames class com.namelessric.dsh_mobile.MainActivity$Companion {
  native <methods>;
}
# dsh-mobile: MLKit bundled 条码解码器（com.google.mlkit:barcode-scanning）。
# thin 调度器按类名反射加载 ThickBarcodeScannerCreator；R8 混淆或移除会导致
# "Failed to load the bundled barcode module"，扫码静默失效。bundled AAR 自带的
# proguard.txt 只保 native 方法与 proto 字段，不覆盖这个反射入口。
-keep class com.google.mlkit.vision.barcode.** { *; }
# JNI 反射调用的 Kotlin 桥（Rust 按类名 loadClass + 方法名调用，R8 不可见）
-keep class com.namelessric.dsh_mobile.InstallBridge { *; }
-keep class com.namelessric.dsh_mobile.ClipboardBridge { *; }
-keep class com.namelessric.dsh_mobile.StatusBarThemeTask { *; }

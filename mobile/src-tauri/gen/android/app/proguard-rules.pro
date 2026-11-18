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
# 审批/提问通知卡片桥（WP4c）：start/stop/onCard 只由 Rust notify.rs 经 JNI
# CallStaticVoidMethod("start"/"onCard"/"stop") 按名字反射调用，Kotlin/Java 侧
# 没有任何调用点。release 开 R8 时这些静态方法会被当死代码删除，只留
# <clinit> 与 nativeAnswer（后者另有 native <methods> 规则保住）→ JNI 抛
# NoSuchMethodError 被 exception_clear() 吞掉，表现为「卡片 delivered:1 但
# 手机不弹」。实测 v1.0.13 装机 APK dex 中 NotifyBridge 仅 [<clinit>,
# nativeAnswer]，而同 APK 内被 keep 的 InstallBridge 方法齐全，故必须显式保活。
-keep class com.namelessric.dsh_mobile.NotifyBridge { *; }
-keep class com.namelessric.dsh_mobile.NotifyBridge$AnswerReceiver { *; }
# 下面是 1.0.25 的真机事故（NoSuchMethodError: no static method listSites）：
# release 是 isMinifyEnabled=true，R8 会把「只被 native 调用」的静态方法当成无用
# 代码删掉/改名 —— Java 侧没有任何引用，R8 无从知道 Rust 在 JNI 里按名字找它。
# 类里所有成员都要保：错误信息只显示类还在、方法没了，最难查。
-keep class com.namelessric.dsh_mobile.CookieBridge { *; }
-keep class com.namelessric.dsh_mobile.CookieBridgeNative { *; }
# 全量清理桥（1.0.30）：wipeAll 同样只由 Rust 经 JNI 按名字调用，R8 看不见
# 任何 Java 侧引用；漏 keep 就是第二个 "NoSuchMethodError: no static method
# wipeAll()Ljava/lang/String;"。它调 CookieBridge.hostsWithCookies 复核，
# 所以 CookieBridge 的 keep 仍然必须（两者一起保）。
-keep class com.namelessric.dsh_mobile.WebDataWipe { *; }
-keep class com.namelessric.dsh_mobile.WebDataWipeNative { *; }
